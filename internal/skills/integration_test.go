package skills_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/skills"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

func setupSkillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// git-helper skill
	gitDir := filepath.Join(dir, "git-helper")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitYAML := "name: git-helper\ndescription: Git version control helper\ntrigger: /git\ncontent_path: ./instructions.md\nallowed_tools:\n  - Read\n  - Bash\n"
	if err := os.WriteFile(filepath.Join(gitDir, "skill.yaml"), []byte(gitYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitContent := "## Git Helper\n\nUse git add, commit, push for version control.\n"
	if err := os.WriteFile(filepath.Join(gitDir, "instructions.md"), []byte(gitContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// code-review skill
	reviewDir := filepath.Join(dir, "code-review")
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewYAML := "name: code-review\ndescription: Code review checklist\ntrigger: /review\ncontent_path: ./review.md\n"
	if err := os.WriteFile(filepath.Join(reviewDir, "skill.yaml"), []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewContent := "## Review Checklist\n\nCheck for bugs, style, tests.\n"
	if err := os.WriteFile(filepath.Join(reviewDir, "review.md"), []byte(reviewContent), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestIntegration_ScanManifestLoadSkill(t *testing.T) {
	dir := setupSkillDir(t)

	// 1. Scan
	cat := skills.Scan(dir)
	if len(cat.Skills) != 2 {
		t.Fatalf("Scan: expected 2 skills, got %d", len(cat.Skills))
	}

	// 2. FormatManifest
	manifest := skills.FormatManifest(cat)
	if !strings.Contains(manifest, "<available_skills>") {
		t.Error("FormatManifest: missing <available_skills>")
	}
	if !strings.Contains(manifest, "- name: code-review") {
		t.Error("FormatManifest: missing name: code-review")
	}
	if !strings.Contains(manifest, "- name: git-helper") {
		t.Error("FormatManifest: missing name: git-helper")
	}
	if !strings.Contains(manifest, "</available_skills>") {
		t.Error("FormatManifest: missing </available_skills>")
	}
	if !strings.Contains(manifest, "LoadSkill") {
		t.Error("FormatManifest: missing LoadSkill")
	}

	ls := skills.NewLoadSkill(cat)
	env := &tools.Env{}

	// 3. LoadSkill git-helper
	res, err := ls.Run(context.Background(), env, json.RawMessage(`{"name":"git-helper"}`))
	if err != nil {
		t.Fatalf("LoadSkill git-helper: unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("LoadSkill git-helper: unexpected IsError")
	}
	if !strings.Contains(res.Output, "Git Helper") {
		t.Errorf("LoadSkill git-helper: output missing 'Git Helper', got %q", res.Output)
	}
	if res.Modifier == nil {
		t.Fatal("LoadSkill git-helper: expected Modifier, got nil")
	}
	rec := &allowedToolsRecorder{}
	if err := res.Modifier.Apply(rec); err != nil {
		t.Fatalf("LoadSkill git-helper: Apply error: %v", err)
	}
	if len(rec.tools) != 2 || rec.tools[0] != "Read" || rec.tools[1] != "Bash" {
		t.Errorf("LoadSkill git-helper: allowed tools = %v, want [Read Bash]", rec.tools)
	}

	// 4. LoadSkill code-review
	res, err = ls.Run(context.Background(), env, json.RawMessage(`{"name":"code-review"}`))
	if err != nil {
		t.Fatalf("LoadSkill code-review: unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("LoadSkill code-review: unexpected IsError")
	}
	if !strings.Contains(res.Output, "Review Checklist") {
		t.Errorf("LoadSkill code-review: output missing 'Review Checklist', got %q", res.Output)
	}
	if res.Modifier != nil {
		t.Errorf("LoadSkill code-review: expected nil Modifier, got %T", res.Modifier)
	}

	// 5. LoadSkill nonexistent
	res, err = ls.Run(context.Background(), env, json.RawMessage(`{"name":"nope"}`))
	if err != nil {
		t.Fatalf("LoadSkill nope: unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("LoadSkill nope: expected IsError")
	}
	if !strings.Contains(res.Output, "skill not found") {
		t.Errorf("LoadSkill nope: output missing 'skill not found', got %q", res.Output)
	}
}
