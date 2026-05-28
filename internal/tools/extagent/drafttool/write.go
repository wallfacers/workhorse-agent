// Package drafttool exposes WriteAdapterDraft, an internal-only tool injected
// into the adapter-generator subagent's surface. It writes a candidate adapter
// YAML to <externalAgentsDir>/.drafts/<safe-name>.yaml after schema-validating
// the content. It is intentionally NOT registered with the global tool
// registry; only the adapter-generator agent type gets it added at session
// start (see internal/coord/loader.go's adapter-generator branch).
//
// The package is named drafttool to keep it distinct from
// internal/extagent/draft, which owns the publish (draft → live) layer.
package drafttool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

// ToolName is the registered name surfaced to the LLM.
const ToolName = "WriteAdapterDraft"

// safeNameRe matches the adapter name embedded in the draft filename. It must
// also match the name field inside the YAML body (verified after parse).
var safeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Host carries the runtime dependencies. ExternalAgentsDir is resolved once at
// server start; the drafts subdirectory is computed lazily on first write.
type Host struct {
	ExternalAgentsDir string
}

// Tool implements tools.Tool.
type Tool struct {
	Host *Host
}

var _ tools.Tool = (*Tool)(nil)

type input struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (Tool) Name() string { return ToolName }

func (Tool) Description() string {
	return "Write a candidate external-agent adapter YAML to the drafts directory. " +
		"Only the adapter-generator subagent has access to this tool. " +
		"Path must be of the form '<externalAgentsDir>/.drafts/<name>.yaml' where " +
		"name matches ^[a-z0-9][a-z0-9_-]{0,63}$ and equals the YAML's `name` field. " +
		"Content is validated against the adapter schema before being written; " +
		"a schema failure returns an error and writes nothing."
}

func (Tool) InputSchema() json.RawMessage {
	schema := map[string]any{
		"type":                 "object",
		"required":             []string{"path", "content"},
		"additionalProperties": false,
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute path under <externalAgentsDir>/.drafts/, ending in <name>.yaml",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Adapter YAML body conforming to the adapter schema",
			},
		},
	}
	data, _ := json.Marshal(schema)
	return data
}

func (Tool) IsReadOnly() bool { return false }

func (Tool) CanRunInParallel() bool { return false }

func (Tool) DefaultTimeout() time.Duration { return 5 * time.Second }

func (t Tool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input JSON: %v", err)
	}
	if t.Host == nil || t.Host.ExternalAgentsDir == "" {
		return errResult("WriteAdapterDraft host misconfigured: ExternalAgentsDir is empty")
	}

	expectedName, err := validateDraftPath(t.Host.ExternalAgentsDir, in.Path)
	if err != nil {
		return errResult("%v", err)
	}

	adapter, err := extagent.Parse([]byte(in.Content))
	if err != nil {
		return errResult("adapter YAML failed schema validation: %v", err)
	}
	if adapter.Name != expectedName {
		return errResult("YAML name=%q does not match filename stem %q", adapter.Name, expectedName)
	}

	if err := os.MkdirAll(pathguard.DraftsDir(t.Host.ExternalAgentsDir), 0o700); err != nil {
		return errResult("create drafts dir: %v", err)
	}

	finalPath, err := pathguard.ResolveDraftForWrite(t.Host.ExternalAgentsDir, expectedName+".yaml")
	if err != nil {
		return errResult("resolve draft path: %v", err)
	}

	if err := atomicWrite(finalPath, []byte(in.Content)); err != nil {
		return errResult("write draft: %v", err)
	}

	return &tools.Result{
		Output: fmt.Sprintf("draft written: %s", finalPath),
	}, nil
}

// validateDraftPath checks the user-supplied path against the safe-name regex
// and confirms the directory layout. Returns the stem on success.
func validateDraftPath(extDir, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute, got %q", path)
	}
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".yaml") {
		return "", fmt.Errorf("path must end in .yaml, got %q", base)
	}
	stem := strings.TrimSuffix(base, ".yaml")
	if !safeNameRe.MatchString(stem) {
		return "", fmt.Errorf("adapter name %q must match ^[a-z0-9][a-z0-9_-]{0,63}$", stem)
	}
	// Path must sit directly in the drafts directory (after Clean, so the
	// trailing slash and any "./" components are normalized). This catches
	// attempts to use absolute paths into the live directory, /etc, etc.
	want := filepath.Clean(filepath.Join(pathguard.DraftsDir(extDir), base))
	got := filepath.Clean(path)
	if want != got {
		return "", fmt.Errorf("path must be %q, got %q", want, got)
	}
	return stem, nil
}

// atomicWrite writes data to a same-directory tempfile (mode 0600) then
// renames it onto path. On Unix, rename within the same directory is atomic
// with respect to readers.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".draft-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func errResult(format string, args ...any) (*tools.Result, error) {
	return &tools.Result{
		Output:  fmt.Sprintf(format, args...),
		IsError: true,
	}, nil
}
