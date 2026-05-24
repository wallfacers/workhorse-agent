// Package bash holds Bash-tool-specific helpers — envfilter, danger guard,
// process-group teardown — that need their own focused tests apart from the
// tool wrapper.
package bash

import (
	"log/slog"
	"strings"

	"github.com/google/shlex"
)

// Source: AI #2 review M-6 plus the Round-3 algorithm clarification. The
// precise rule set:
//
//   - Exact-match block list for shared-library and Python preload vectors.
//   - Anything starting with DYLD_ is dropped (catches future macOS
//     additions without us having to chase them).
//   - NODE_OPTIONS is shlex-tokenised (POSIX rules); a single dangerous
//     token taints the whole variable. We do not try to keep the safe tokens
//     because partial-NODE_OPTIONS sub-environments are surprising to
//     debug — if the variable carries any dangerous token, the whole
//     variable is removed.

// exactDeny is the exhaustive list of env variable names we never let through.
var exactDeny = map[string]struct{}{
	"LD_PRELOAD":                  {},
	"LD_LIBRARY_PATH":             {},
	"LD_AUDIT":                    {},
	"DYLD_INSERT_LIBRARIES":       {},
	"DYLD_LIBRARY_PATH":           {},
	"DYLD_FALLBACK_LIBRARY_PATH":  {},
	"DYLD_FORCE_FLAT_NAMESPACE":   {},
	"PYTHONPATH":                  {},
	"PYTHONSTARTUP":               {},
}

// nodeOptionDanger lists the NODE_OPTIONS sub-flags that can load arbitrary
// code into the child. Token-level prefix match: e.g. "--require=foo.js"
// matches "--require".
var nodeOptionDanger = []string{
	"--require",
	"--import",
	"--experimental-loader",
	"--inspect",
	"--inspect-brk",
}

// Filter walks env (KEY=VALUE strings, as produced by os.Environ()) and
// returns the kept entries plus the dropped *keys* (values are intentionally
// not surfaced so a Bearer token in an env doesn't leak via the warn log).
func Filter(env []string) (kept []string, dropped []string) {
	kept = make([]string, 0, len(env))
	for _, e := range env {
		key, _ := splitKV(e)
		if !isAllowed(key, valueOf(e)) {
			dropped = append(dropped, key)
			continue
		}
		kept = append(kept, e)
	}
	return kept, dropped
}

// FilterMap applies the same rules to a key/value map. Returns the kept map
// and the dropped key set so callers can warn.
func FilterMap(in map[string]string) (map[string]string, []string) {
	out := make(map[string]string, len(in))
	var dropped []string
	for k, v := range in {
		if !isAllowed(k, v) {
			dropped = append(dropped, k)
			continue
		}
		out[k] = v
	}
	return out, dropped
}

// LogDropped emits a structured warn for every dropped key. Values are never
// included — even if the operator set the variable themselves, an env that
// looks innocent can carry secrets.
func LogDropped(logger *slog.Logger, dropped []string) {
	if logger == nil || len(dropped) == 0 {
		return
	}
	logger.Warn("dropped dangerous env from Bash execution",
		slog.Any("keys", dropped))
}

// isAllowed returns whether (key, value) is safe to pass into a Bash child.
func isAllowed(key, value string) bool {
	if _, ok := exactDeny[key]; ok {
		return false
	}
	if strings.HasPrefix(key, "DYLD_") {
		return false
	}
	if key == "NODE_OPTIONS" {
		return nodeOptionsSafe(value)
	}
	return true
}

// nodeOptionsSafe shlex-tokenises NODE_OPTIONS and rejects the whole variable
// if any token starts with a dangerous flag.
func nodeOptionsSafe(value string) bool {
	tokens, err := shlex.Split(value)
	if err != nil {
		// shlex couldn't parse — be conservative.
		return false
	}
	for _, t := range tokens {
		for _, bad := range nodeOptionDanger {
			if t == bad || strings.HasPrefix(t, bad+"=") {
				return false
			}
		}
	}
	return true
}

// splitKV splits a "KEY=VALUE" entry. KEY is the part before the first '='.
func splitKV(e string) (string, string) {
	i := strings.IndexByte(e, '=')
	if i < 0 {
		return e, ""
	}
	return e[:i], e[i+1:]
}

// valueOf returns the part after '=' or an empty string.
func valueOf(e string) string {
	_, v := splitKV(e)
	return v
}
