package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/memory/curation"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// permReloader re-applies the hot-reloadable permission subset of config.yaml
// at runtime. It owns the last-applied config snapshot so it can diff against a
// freshly loaded one and only act on the permission fields.
type permReloader struct {
	configPath string
	args       []string // same CLI args as startup, so flag overrides re-apply
	lookupEnv  func(string) (string, bool)
	store      *sqlite.Store
	perm       *permission.Manager
	curator    *curation.Worker // nil-safe: SetHotConfig is only called when non-nil
	logger     *slog.Logger

	mu      sync.Mutex
	current config.Config
}

func newPermReloader(configPath string, current config.Config, st *sqlite.Store, perm *permission.Manager, curator *curation.Worker, logger *slog.Logger) *permReloader {
	return &permReloader{
		configPath: configPath,
		store:      st,
		perm:       perm,
		curator:    curator,
		logger:     logger,
		current:    current,
	}
}

// Reload re-loads config.yaml, applies any changed permission-subset fields,
// and warns about changed non-reloadable fields. On parse/validation failure
// the previously-applied config is kept untouched (fail-safe) and the error is
// returned after a WARN.
func (r *permReloader) Reload(ctx context.Context) error {
	newCfg, err := config.Load(config.LoadOptions{
		YAMLPath:         r.configPath,
		Args:             r.args,
		LookupEnv:        r.lookupEnv,
		ResolveHomePaths: true,
	})
	if err != nil {
		r.logger.Warn("config reload: invalid config, keeping previous", "error", err)
		return err
	}

	r.mu.Lock()
	old := r.current
	r.mu.Unlock()

	diff := config.DiffReloadable(old, newCfg)

	if diff.PresetRulesChanged {
		if aerr := applyPresetRules(ctx, r.store, newCfg.Tools.PresetRules, r.logger); aerr != nil {
			r.logger.Warn("config reload: applying preset rules failed, keeping previous", "error", aerr)
			return aerr
		}
	}
	if diff.DefaultPermissionChanged {
		r.perm.SetDefaultDecision(store.PermissionDecision(newCfg.Tools.DefaultPermission))
	}
	if diff.TimeoutChanged {
		r.perm.SetTimeout(time.Duration(newCfg.Agent.PermissionRequestTimeoutSeconds) * time.Second)
	}
	if diff.CurationChanged && r.curator != nil {
		cur := newCfg.Memory.Curation
		r.curator.SetHotConfig(
			cur.EntryCountHigh,
			time.Duration(cur.MinIntervalMinutes)*time.Minute,
			time.Duration(cur.LeaseTTLSeconds)*time.Second,
		)
	}
	for _, f := range diff.NonReloadable {
		r.logger.Warn("config reload: field changed; restart required to take effect", "field", f)
	}

	r.mu.Lock()
	r.current = newCfg
	r.mu.Unlock()

	if diff.HasReloadable() {
		r.logger.Info("config reload: applied runtime changes",
			"preset_rules", diff.PresetRulesChanged,
			"default_permission", diff.DefaultPermissionChanged,
			"timeout", diff.TimeoutChanged,
			"curation", diff.CurationChanged)
	}
	return nil
}

// defaultReloadDebounce coalesces the burst of filesystem events an editor's
// "write temp + rename" save produces into a single reload.
const defaultReloadDebounce = 200 * time.Millisecond

// watchConfigFile watches the directory containing configPath and invokes
// onChange (debounced) whenever that file is created or written. Watching the
// directory — not the file inode — is required because atomic saves replace the
// file via rename, which a fixed-inode watch would stop seeing. The returned
// stop function closes the watcher and waits for the goroutine to exit; the
// watcher also stops when ctx is cancelled.
func watchConfigFile(ctx context.Context, configPath string, debounce time.Duration, onChange func(), logger *slog.Logger) (func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(configPath)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	target := filepath.Clean(configPath)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var timer *time.Timer
		var timerC <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				_ = w.Close()
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != target {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(debounce)
				timerC = timer.C
			case <-timerC:
				timerC = nil
				onChange()
			case werr, ok := <-w.Errors:
				if !ok {
					return
				}
				logger.Warn("config watch: watcher error", "error", werr)
			}
		}
	}()

	return func() {
		_ = w.Close()
		<-done
	}, nil
}
