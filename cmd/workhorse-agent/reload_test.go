package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newReloader(t *testing.T, configYAML string, st *sqlite.Store, perm *permission.Manager, logger *slog.Logger) (*permReloader, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cur, err := config.Load(config.LoadOptions{YAMLPath: path, ResolveHomePaths: true})
	if err != nil {
		t.Fatal(err)
	}
	// Mirror startup: preset rules are reconciled into the store before the
	// reloader takes over, and its snapshot reflects that applied state.
	if err := applyPresetRules(context.Background(), st, cur.Tools.PresetRules, logger); err != nil {
		t.Fatal(err)
	}
	return newPermReloader(path, cur, st, perm, nil, logger), path
}

func newManager(st *sqlite.Store, def store.PermissionDecision) *permission.Manager {
	return permission.New(st,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			return permission.Deny, true
		}, nil, 0, def)
}

// 4.1 — a freshly added preset rule is reconciled into the store on reload.
func TestReload_AppliesNewPresetRule(t *testing.T) {
	st := openMemStore(t)
	perm := newManager(st, "")
	r, path := newReloader(t, "tools:\n  preset_rules: []\n", st, perm, discardLogger())

	if len(permRows(t, st)) != 0 {
		t.Fatalf("expected no rules initially")
	}

	const updated = `tools:
  preset_rules:
    - tool: Bash
      pattern: "rm *"
      decision: deny_permanent
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows := permRows(t, st)
	if len(rows) != 1 || rows[0].Tool != "Bash" || rows[0].Decision != store.DecisionDenyPermanent {
		t.Fatalf("preset not applied on reload, rows=%+v", rows)
	}

	// And the running manager honors it immediately on the next Check.
	d, _, err := perm.Check(context.Background(), permission.CheckInput{SessionID: "sess", Tool: "Bash", Resource: "rm tmp"})
	if err != nil || d != permission.DenyPermanent {
		t.Fatalf("Check after reload = (%v,%v), want deny_permanent", d, err)
	}
}

// 4.2 — an invalid config is rejected and the previously-applied rules stand.
func TestReload_InvalidConfigKeepsPrevious(t *testing.T) {
	st := openMemStore(t)
	perm := newManager(st, "")
	const initial = `tools:
  preset_rules:
    - tool: Read
      pattern: "/safe/**"
      decision: allow_permanent
`
	r, path := newReloader(t, initial, st, perm, discardLogger())
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(permRows(t, st)) != 1 {
		t.Fatalf("setup: expected 1 rule, got %d", len(permRows(t, st)))
	}

	// Invalid decision → config.Validate rejects → reload must be a no-op.
	const broken = `tools:
  preset_rules:
    - tool: Read
      pattern: "/safe/**"
      decision: bogus_value
`
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err == nil {
		t.Fatal("expected reload to reject invalid config")
	}
	rows := permRows(t, st)
	if len(rows) != 1 || rows[0].Decision != store.DecisionAllowPermanent {
		t.Fatalf("previous rule must survive an invalid reload, rows=%+v", rows)
	}
}

// 4.3 — a changed non-reloadable field is ignored but logged at WARN.
func TestReload_NonReloadableFieldWarns(t *testing.T) {
	st := openMemStore(t)
	perm := newManager(st, "")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r, path := newReloader(t, "server:\n  port: 7821\n", st, perm, logger)

	if err := os.WriteFile(path, []byte("server:\n  port: 9000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("reload should succeed (port change is ignored, not fatal): %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("server.port")) {
		t.Errorf("expected WARN naming server.port, got logs:\n%s", buf.String())
	}
}

// watcher fires onChange (debounced) when the target file is written.
func TestWatchConfigFile_FiresOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 7821\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	calls := 0
	fired := make(chan struct{}, 1)
	onChange := func() {
		mu.Lock()
		calls++
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := watchConfigFile(ctx, path, 50*time.Millisecond, onChange, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if err := os.WriteFile(path, []byte("server:\n  port: 9000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire onChange after a write")
	}
}
