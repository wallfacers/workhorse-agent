package extagent

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Non-recursive glob: add-llm-adapter-generator places agent-type YAMLs in builtins/agents/;
// a recursive glob would pick those up and reject as schema-invalid every startup.
//go:embed builtins/*.yaml
var builtinFS embed.FS

var filenameStemRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Snapshot is an immutable registry snapshot. Created once per session.
type Snapshot struct {
	adapters map[string]*Adapter
}

// Registry wraps a Snapshot for querying.
type Registry struct {
	snap *Snapshot
}

// Loader builds a Registry by merging embedded builtins with on-disk adapters.
type Loader struct {
	Logger *slog.Logger
}

// NewSnapshot creates a Snapshot from a list of adapters.
func NewSnapshot(adapters []*Adapter) *Snapshot {
	m := make(map[string]*Adapter, len(adapters))
	for _, a := range adapters {
		m[a.Name] = a
	}
	return &Snapshot{adapters: m}
}

// NewRegistry creates a Registry from a snapshot.
func NewRegistry(snap *Snapshot) *Registry {
	return &Registry{snap: snap}
}

// Snapshot returns the underlying snapshot.
func (r *Registry) Snapshot() *Snapshot { return r.snap }

// Get returns an adapter by name, or nil.
func (r *Registry) Get(name string) *Adapter {
	if r.snap == nil {
		return nil
	}
	return r.snap.adapters[name]
}

// All returns all adapters.
func (r *Registry) All() []*Adapter {
	if r.snap == nil {
		return nil
	}
	out := make([]*Adapter, 0, len(r.snap.adapters))
	for _, a := range r.snap.adapters {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Healthy returns adapters that are not BinaryMissing and have passed smoke.
func (r *Registry) Healthy() []*Adapter {
	if r.snap == nil {
		return nil
	}
	out := make([]*Adapter, 0, len(r.snap.adapters))
	for _, a := range r.snap.adapters {
		if a.IsHealthy() {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HealthySubAgents returns healthy sub_agent-class adapters only.
func (r *Registry) HealthySubAgents() []*Adapter {
	if r.snap == nil {
		return nil
	}
	out := make([]*Adapter, 0)
	for _, a := range r.snap.adapters {
		if a.Class == ClassSubAgent && a.IsHealthy() {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns sorted adapter names.
func (r *Registry) Names() []string {
	if r.snap == nil {
		return nil
	}
	out := make([]string, 0, len(r.snap.adapters))
	for name := range r.snap.adapters {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Adapters returns all adapters (for smoke testing, etc).
func (r *Registry) Adapters() []*Adapter {
	if r.snap == nil {
		return nil
	}
	return r.All()
}

// Load scans dir for adapter YAMLs, merging with embedded builtins.
func (l *Loader) Load(dir string) (*Snapshot, error) {
	adapters := make(map[string]*Adapter)

	// 1. Seed with embedded builtins.
	if err := l.loadBuiltins(adapters); err != nil {
		l.logger().Warn("extagent: failed to load builtins", "err", err)
	}

	// 2. Ensure directory exists. Best-effort: a read-only HOME or EROFS
	// must not wipe out the embedded builtins, so we log and proceed with
	// whatever was seeded above.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.logger().Warn("extagent: failed to create adapter dir, builtins only",
			"dir", dir, "err", err)
		return &Snapshot{adapters: adapters}, nil
	}

	// 3. Load on-disk adapters (override builtins by name).
	entries, err := os.ReadDir(dir)
	if err != nil {
		l.logger().Warn("extagent: failed to read adapter dir, builtins only",
			"dir", dir, "err", err)
		return &Snapshot{adapters: adapters}, nil
	}
	for _, ent := range entries {
		// add-llm-adapter-generator: hidden subdirs hold in-flight drafts
		// (notably .drafts/). They MUST NEVER be loaded into a session's
		// registry — drafts only become adapters via atomic rename after
		// approval. The IsDir check below already skips them; this guard
		// makes the intent explicit and survives accidental flattening.
		if strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		stem := strings.TrimSuffix(ent.Name(), ".yaml")
		if !filenameStemRe.MatchString(stem) {
			l.logger().Error("extagent: filename rejected (must be lowercase kebab-case)",
				"file", ent.Name(), "pattern", filenameStemRe.String())
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			l.logger().Warn("extagent: failed to read file", "file", ent.Name(), "err", err)
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			l.logger().Warn("extagent: failed to parse adapter", "file", ent.Name(), "err", err)
			continue
		}
		if a.Name != stem {
			l.logger().Warn("extagent: filename stem does not match name field",
				"file", ent.Name(), "stem", stem, "name", a.Name)
			continue
		}
		if err := resolveBinary(a); err != nil {
			l.logger().Warn("extagent: binary resolution failed",
				"adapter", a.Name, "binary", a.Binary, "err", err)
			a.BinaryMissing = true
		}
		a.LoadedAt = time.Now()
		adapters[a.Name] = a
	}

	return &Snapshot{adapters: adapters}, nil
}

func (l *Loader) loadBuiltins(adapters map[string]*Adapter) error {
	entries, err := builtinFS.ReadDir("builtins")
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		raw, err := builtinFS.ReadFile(filepath.Join("builtins", ent.Name()))
		if err != nil {
			return err
		}
		a, err := Parse(raw)
		if err != nil {
			return fmt.Errorf("builtin %s: %w", ent.Name(), err)
		}
		if err := resolveBinary(a); err != nil {
			a.BinaryMissing = true
		}
		a.LoadedAt = time.Now()
		adapters[a.Name] = a
	}
	return nil
}

func resolveBinary(a *Adapter) error {
	if filepath.IsAbs(a.Binary) {
		fi, err := os.Stat(a.Binary)
		if err != nil {
			a.BinaryMissing = true
			a.ResolvedBinary = a.Binary
			return err
		}
		if fi.Mode()&0o111 == 0 {
			a.BinaryMissing = true
			a.ResolvedBinary = a.Binary
			return fmt.Errorf("not executable: %s", a.Binary)
		}
		a.ResolvedBinary = a.Binary
		a.BinaryMissing = false
		return nil
	}
	resolved, err := exec.LookPath(a.Binary)
	if err != nil {
		a.BinaryMissing = true
		a.ResolvedBinary = a.Binary
		return err
	}
	a.ResolvedBinary = resolved
	a.BinaryMissing = false
	return nil
}

func (l *Loader) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}
