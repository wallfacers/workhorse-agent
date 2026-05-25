package skills_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wallfacers/workhorse-agent/internal/skills"
)

func writeSkillYAML(t *testing.T, dir, name, desc, trigger, contentPath string, allowedTools []string) {
	t.Helper()
	writeSkillDir(t, dir, name, name, desc, trigger, contentPath, allowedTools)
}

func writeSkillDir(t *testing.T, baseDir, dirName, skillName, desc, trigger, contentPath string, allowedTools []string) {
	t.Helper()
	skillDir := filepath.Join(baseDir, dirName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	cfg := struct {
		Name         string   `yaml:"name"`
		Description  string   `yaml:"description"`
		Trigger      string   `yaml:"trigger"`
		ContentPath  string   `yaml:"content_path"`
		AllowedTools []string `yaml:"allowed_tools"`
	}{
		Name:         skillName,
		Description:  desc,
		Trigger:      trigger,
		ContentPath:  contentPath,
		AllowedTools: allowedTools,
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), data, 0o644); err != nil {
		t.Fatalf("write skill.yaml: %v", err)
	}
}

func writeInstructions(t *testing.T, dir, skillName, content string) {
	t.Helper()
	path := filepath.Join(dir, skillName, "instructions.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write instructions: %v", err)
	}
}

func TestScan_TwoSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkillYAML(t, dir, "alpha", "Alpha skill", "/alpha", "instructions.md", nil)
	writeInstructions(t, dir, "alpha", "You are alpha.")
	writeSkillYAML(t, dir, "bravo", "Bravo skill", "/bravo", "instructions.md", nil)
	writeInstructions(t, dir, "bravo", "You are bravo.")

	cat := skills.Scan(dir)
	if len(cat.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(cat.Skills))
	}
	if cat.Skills[0].Name != "alpha" {
		t.Errorf("first skill = %q, want alpha", cat.Skills[0].Name)
	}
	if cat.Skills[1].Name != "bravo" {
		t.Errorf("second skill = %q, want bravo", cat.Skills[1].Name)
	}
	if cat.Skills[0].Content != "You are alpha." {
		t.Errorf("alpha content = %q", cat.Skills[0].Content)
	}
	if cat.Skills[1].Content != "You are bravo." {
		t.Errorf("bravo content = %q", cat.Skills[1].Content)
	}
}

func TestScan_MissingContentPath(t *testing.T) {
	dir := t.TempDir()
	writeSkillYAML(t, dir, "orphan", "No file", "/orphan", "missing.md", nil)

	cat := skills.Scan(dir)
	if len(cat.Skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(cat.Skills))
	}
}

func TestScan_DuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeSkillDir(t, dir, "aaa", "dup", "dup desc", "/dup", "instructions.md", nil)
	writeInstructions(t, dir, "aaa", "first wins")
	writeSkillDir(t, dir, "bbb", "dup", "dup desc", "/dup", "instructions.md", nil)
	writeInstructions(t, dir, "bbb", "second loses")

	cat := skills.Scan(dir)
	if len(cat.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(cat.Skills))
	}
	if cat.Skills[0].Content != "first wins" {
		t.Errorf("content = %q, want first wins", cat.Skills[0].Content)
	}
}

func TestScan_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cat := skills.Scan(dir)
	if len(cat.Skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(cat.Skills))
	}
}

func TestScan_NonexistentDir(t *testing.T) {
	cat := skills.Scan("/no/such/directory")
	if len(cat.Skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(cat.Skills))
	}
}

func TestScan_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "broken")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte("{{{{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := skills.Scan(dir)
	if len(cat.Skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(cat.Skills))
	}
}

func TestScan_AllowedTools(t *testing.T) {
	dir := t.TempDir()
	tools := []string{"Read", "Write", "Bash"}
	writeSkillYAML(t, dir, "tooled", "Has tools", "/tooled", "instructions.md", tools)
	writeInstructions(t, dir, "tooled", "use tools")

	cat := skills.Scan(dir)
	if len(cat.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(cat.Skills))
	}
	s := cat.Skills[0]
	if len(s.AllowedTools) != 3 {
		t.Fatalf("expected 3 allowed tools, got %d", len(s.AllowedTools))
	}
	for i, want := range tools {
		if s.AllowedTools[i] != want {
			t.Errorf("AllowedTools[%d] = %q, want %q", i, s.AllowedTools[i], want)
		}
	}
}

func TestScan_DuplicateFirstFailsSecondSkipped(t *testing.T) {
	dir := t.TempDir()
	// aaa has duplicate name but missing content file — name is claimed.
	aDir := filepath.Join(dir, "aaa")
	os.MkdirAll(aDir, 0o755)
	os.WriteFile(filepath.Join(aDir, "skill.yaml"), []byte(
		"name: helper\ndescription: first\ntrigger: first\ncontent_path: ./missing.md\n"), 0o644)
	// bbb has same name but valid content — skipped as duplicate.
	bDir := filepath.Join(dir, "bbb")
	os.MkdirAll(bDir, 0o755)
	os.WriteFile(filepath.Join(bDir, "skill.yaml"), []byte(
		"name: helper\ndescription: second\ntrigger: second\ncontent_path: ./ok.md\n"), 0o644)
	os.WriteFile(filepath.Join(bDir, "ok.md"), []byte("content-b"), 0o644)

	cat := skills.Scan(dir)
	if len(cat.Skills) != 0 {
		t.Fatalf("expected 0 skills (first failed, second skipped as dup), got %d", len(cat.Skills))
	}
}

func TestScan_ContentPathTraversal(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "evil")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "name: evil\ndescription: escapes\ntrigger: /evil\ncontent_path: ../../etc/passwd\n"
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := skills.Scan(dir)
	if len(cat.Skills) != 0 {
		t.Fatalf("expected 0 skills (path traversal blocked), got %d", len(cat.Skills))
	}
}

func TestCatalog_Get(t *testing.T) {
	dir := t.TempDir()
	writeSkillYAML(t, dir, "findme", "desc", "/findme", "instructions.md", nil)
	writeInstructions(t, dir, "findme", "hello")

	cat := skills.Scan(dir)
	s := cat.Get("findme")
	if s == nil {
		t.Fatal("Get(findme) = nil, want skill")
	}
	if s.Content != "hello" {
		t.Errorf("Content = %q, want hello", s.Content)
	}
	if cat.Get("nope") != nil {
		t.Error("Get(nope) should be nil")
	}
}
