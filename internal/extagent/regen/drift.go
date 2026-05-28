// Package regen owns drift detection — surfacing the case where a binary
// underlying an llm_generated adapter has changed version since the adapter
// was created. The check runs at server start (log + diagnostics surface
// only; never auto-regenerates).
package regen

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
)

// versionProbeTimeout caps each `<binary> --version` invocation. Some CLIs
// hang or print interactively on stdin, so a strict per-probe deadline is
// non-negotiable. Two seconds is generous for any well-behaved CLI.
const versionProbeTimeout = 2 * time.Second

// Entry describes a single drift instance — useful for log lines and the
// /diagnostics endpoint.
type Entry struct {
	Name           string `json:"name"`
	AdapterPath    string `json:"adapter_path,omitempty"`
	Was            string `json:"was"`
	Now            string `json:"now"`
	BinaryResolved string `json:"binary"`
}

// Check inspects every llm_generated adapter in reg and returns drift entries
// for those whose current `<binary> --version` output disagrees with the
// provenance.tool_version captured at generation time.
//
// Per design.md G3 / §11.1 task wording: when provenance.tool_version is
// empty (the original probe failed), comparison is SKIPPED — flagging would
// produce spurious entries on every restart. Binary-missing adapters are
// also skipped because Change 1's loader already marks them.
func Check(reg *extagent.Registry, logger *slog.Logger) []Entry {
	if reg == nil {
		return nil
	}
	var out []Entry
	for _, a := range reg.All() {
		if a == nil || a.Provenance.Source != "llm_generated" {
			continue
		}
		if a.Provenance.ToolVersion == "" {
			continue
		}
		if a.BinaryMissing {
			continue
		}
		current := probeVersion(a.ResolvedBinary)
		if current == "" {
			continue
		}
		// Treat the comparison as an exact string match. CLIs sometimes
		// embed the version inside a longer banner ("foo v1.2.3 (build
		// abc)"), so we accept "stored is a substring of current" as
		// no-drift; only a true mismatch becomes an entry.
		if strings.Contains(current, a.Provenance.ToolVersion) {
			continue
		}
		entry := Entry{
			Name:           a.Name,
			Was:            a.Provenance.ToolVersion,
			Now:            current,
			BinaryResolved: a.ResolvedBinary,
		}
		if logger != nil {
			logger.Info("external_agent.drift",
				"name", entry.Name,
				"was", entry.Was,
				"now", entry.Now,
				"binary", entry.BinaryResolved)
		}
		out = append(out, entry)
	}
	return out
}

// probeVersion runs `<bin> --version` with a strict timeout and returns the
// captured stdout (trimmed) or "" on any failure. We don't error-propagate
// because the caller is a startup log line — a failed probe is a missing
// data point, not a system error.
func probeVersion(bin string) string {
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
