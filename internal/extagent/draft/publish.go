// Package draft owns the "draft → live" publish step: re-validating the YAML
// on disk one final time, renaming it onto the live adapter directory, and
// writing a sibling .genmeta file capturing the audit trail (model id, raw
// --help output, timestamps).
//
// This package is intentionally separate from
// internal/tools/extagent/drafttool — that one owns the WRITE side
// (subagent → drafts dir). This one owns the PUBLISH side (drafts dir →
// live dir). Keeping them in different packages eliminates the "two draft
// packages" import collision the design warned about.
package draft

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
)

// GenmetaExt is the file suffix for the audit-trail sibling written next to
// every published llm_generated adapter. Exported so drift checks and other
// auditors can spell it consistently.
const GenmetaExt = ".genmeta"

// adapterStemRE mirrors the regex used by extagent's loader to validate
// on-disk filenames. Both must agree or a published draft could fail to load.
var adapterStemRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// GenmetaPayload is the audit-trail content written alongside the published
// adapter as <name>.genmeta. The file is human-readable JSON, mode 0600.
type GenmetaPayload struct {
	GeneratedBy   string    `json:"generated_by"`
	GeneratedAt   time.Time `json:"generated_at"`
	ToolVersion   string    `json:"tool_version"`
	Binary        string    `json:"binary"`
	Prompt        string    `json:"prompt"`
	HelpOutput    string    `json:"help_output"`
	VersionOutput string    `json:"version_output,omitempty"`
	ManOutput     string    `json:"man_output,omitempty"`
}

// Publisher knows where the live adapter directory is and writes drafts into
// it. Construct one per server start; safe for concurrent use — os.Rename
// is independently atomic on POSIX.
type Publisher struct {
	LiveDir string
}

// Publish re-validates the YAML on disk one final time, renames it onto the
// live directory atomically, and writes the sibling .genmeta file. Returns
// the resulting live path on success.
//
// Errors are returned without renaming anything if validation fails. The
// .genmeta file is best-effort: if it can't be written (e.g. EROFS on the
// live dir after rename), the adapter is still considered published and the
// caller MUST NOT roll back the rename (an unwound rename would create a
// worse race).
func (p *Publisher) Publish(draftPath string, genmeta GenmetaPayload) (string, error) {
	if p == nil || p.LiveDir == "" {
		return "", errors.New("draft.Publish: LiveDir not configured")
	}
	raw, err := os.ReadFile(draftPath)
	if err != nil {
		return "", fmt.Errorf("draft.Publish: read draft: %w", err)
	}
	adapter, err := extagent.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("draft.Publish: re-validation failed: %w", err)
	}
	if !adapterStemRE.MatchString(adapter.Name) {
		return "", fmt.Errorf("draft.Publish: adapter name %q is not a valid filename stem", adapter.Name)
	}
	livePath := filepath.Join(p.LiveDir, adapter.Name+".yaml")
	if !sameFilesystem(filepath.Dir(draftPath), p.LiveDir) {
		return "", fmt.Errorf("draft.Publish: drafts and live dirs are on different filesystems")
	}
	if err := os.Rename(draftPath, livePath); err != nil {
		return "", fmt.Errorf("draft.Publish: rename: %w", err)
	}
	_ = os.Chmod(livePath, 0o600)

	if err := writeGenmeta(p.LiveDir, adapter.Name, genmeta); err != nil {
		return livePath, fmt.Errorf("draft.Publish: genmeta write failed (adapter still published): %w", err)
	}
	return livePath, nil
}

func writeGenmeta(liveDir, name string, payload GenmetaPayload) error {
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	dst := filepath.Join(liveDir, name+GenmetaExt)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// sameFilesystem returns true when a and b live on the same filesystem (by
// device id). The platform-specific implementation lives in publish_unix.go
// (and would in publish_windows.go if Windows support landed).
func sameFilesystem(a, b string) bool {
	return fsDevice(a) == fsDevice(b)
}
