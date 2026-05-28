package draft_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent/draft"
)

// validYAML is the smallest schema-valid adapter for these tests.
func validYAML(name string) string {
	return `name: ` + name + `
binary: ` + name + `
class: sub_agent
invocation:
  prompt_via: stdin
  extra_args: []
  env_passthrough: []
output:
  format: text
  stderr: separate
control:
  cancel_signal: SIGINT
  cancel_grace_sec: 5
  default_timeout_sec: 600
  max_timeout_sec: 3600
security:
  network: allowed
  filesystem: full
  trusted: false
smoke_test:
  prompt: "Reply with exactly: WORKHORSE_SMOKE_OK"
  expected_substring: "WORKHORSE_SMOKE_OK"
  timeout_sec: 60
description: "test fixture"
provenance:
  source: llm_generated
`
}

func setupLayout(t *testing.T) (liveDir, draftPath string) {
	t.Helper()
	liveDir = t.TempDir()
	drafts := filepath.Join(liveDir, ".drafts")
	if err := os.MkdirAll(drafts, 0o700); err != nil {
		t.Fatal(err)
	}
	draftPath = filepath.Join(drafts, "gemini.yaml")
	if err := os.WriteFile(draftPath, []byte(validYAML("gemini")), 0o600); err != nil {
		t.Fatal(err)
	}
	return liveDir, draftPath
}

func makeGenmeta() draft.GenmetaPayload {
	return draft.GenmetaPayload{
		GeneratedBy: "anthropic:claude-opus-4-7",
		GeneratedAt: time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
		ToolVersion: "0.4.1",
		Binary:      "/usr/local/bin/gemini",
		Prompt:      "<rendered prompt>",
		HelpOutput:  "Usage: gemini ...",
	}
}

func TestPublish_RenamesAtomicallyWithinFS(t *testing.T) {
	liveDir, draftPath := setupLayout(t)
	pub := &draft.Publisher{LiveDir: liveDir}
	livePath, err := pub.Publish(draftPath, makeGenmeta())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := os.Stat(draftPath); !os.IsNotExist(err) {
		t.Error("draft should be gone after rename")
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("live file missing: %v", err)
	}
	if filepath.Base(livePath) != "gemini.yaml" {
		t.Errorf("live filename: got %s, want gemini.yaml", filepath.Base(livePath))
	}
	info, _ := os.Stat(livePath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("live file mode: got %o, want 0600", info.Mode().Perm())
	}
}

func TestPublish_WritesGenmetaSibling(t *testing.T) {
	liveDir, draftPath := setupLayout(t)
	pub := &draft.Publisher{LiveDir: liveDir}
	if _, err := pub.Publish(draftPath, makeGenmeta()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	genmetaPath := filepath.Join(liveDir, "gemini"+draft.GenmetaExt)
	data, err := os.ReadFile(genmetaPath)
	if err != nil {
		t.Fatalf("read genmeta: %v", err)
	}
	info, _ := os.Stat(genmetaPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("genmeta mode: got %o, want 0600", info.Mode().Perm())
	}
	var payload draft.GenmetaPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("genmeta not valid JSON: %v", err)
	}
	if payload.GeneratedBy != "anthropic:claude-opus-4-7" {
		t.Errorf("generated_by: %q", payload.GeneratedBy)
	}
	if payload.ToolVersion != "0.4.1" {
		t.Errorf("tool_version: %q", payload.ToolVersion)
	}
}

func TestPublish_RejectsInvalidYAML(t *testing.T) {
	liveDir := t.TempDir()
	drafts := filepath.Join(liveDir, ".drafts")
	_ = os.MkdirAll(drafts, 0o700)
	bad := filepath.Join(drafts, "broken.yaml")
	_ = os.WriteFile(bad, []byte("name: broken\nbinary: ''\n"), 0o600)

	pub := &draft.Publisher{LiveDir: liveDir}
	if _, err := pub.Publish(bad, makeGenmeta()); err == nil {
		t.Error("invalid YAML should be rejected at publish time")
	}
	// Draft must NOT be moved on validation failure.
	if _, err := os.Stat(bad); err != nil {
		t.Errorf("draft should remain in place on validation failure: %v", err)
	}
}

func TestPublish_IdempotentOnConcurrentSecondCall(t *testing.T) {
	// Once the first Publish renames the draft, a second call must fail
	// cleanly (file no longer at draft path) rather than corrupt the live
	// file. We simulate this by Publish-ing then calling Publish again.
	liveDir, draftPath := setupLayout(t)
	pub := &draft.Publisher{LiveDir: liveDir}
	if _, err := pub.Publish(draftPath, makeGenmeta()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if _, err := pub.Publish(draftPath, makeGenmeta()); err == nil {
		t.Error("second Publish should fail (draft already moved)")
	}
}

func TestPublish_RequiresLiveDir(t *testing.T) {
	pub := &draft.Publisher{LiveDir: ""}
	if _, err := pub.Publish("/tmp/x", makeGenmeta()); err == nil {
		t.Error("empty LiveDir should be rejected")
	} else if !strings.Contains(err.Error(), "LiveDir") {
		t.Errorf("error should mention LiveDir: %v", err)
	}
}
