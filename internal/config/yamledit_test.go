package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfigYAML = `# workhorse-agent config
server:
  host: 127.0.0.1   # loopback only
  port: 7821

# === auth ===
auth:
  enabled: false

tools:
  default_permission: ""
  default_timeout_seconds: 60
  preset_rules:
    - tool: Bash
      pattern: "git *"
      decision: allow_permanent

logging:
  level: info
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadPermissionConfig(t *testing.T) {
	p := writeTemp(t, sampleConfigYAML)
	pc, err := ReadPermissionConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if pc.DefaultPermission != "" {
		t.Errorf("default_permission = %q, want empty", pc.DefaultPermission)
	}
	if len(pc.PresetRules) != 1 {
		t.Fatalf("preset_rules len = %d, want 1", len(pc.PresetRules))
	}
	got := pc.PresetRules[0]
	if got.Tool != "Bash" || got.Pattern != "git *" || got.Decision != "allow_permanent" {
		t.Errorf("rule = %+v, want {Bash, git *, allow_permanent}", got)
	}
}

func TestReadPermissionConfig_MissingFile(t *testing.T) {
	pc, err := ReadPermissionConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if pc.DefaultPermission != "" || len(pc.PresetRules) != 0 {
		t.Errorf("missing file should yield empty config, got %+v", pc)
	}
}

func TestWritePermissionConfig_PreservesComments(t *testing.T) {
	p := writeTemp(t, sampleConfigYAML)

	newPC := PermissionConfig{
		DefaultPermission: "deny_permanent",
		PresetRules: []PresetRule{
			{Tool: "Read", Pattern: "/etc/**", Decision: "deny_permanent"},
			{Tool: "Bash", Pattern: "npm *", Decision: "allow_permanent"},
		},
	}
	if err := WritePermissionConfig(p, newPC); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// Other sections and their comments survive untouched.
	for _, want := range []string{
		"# workhorse-agent config",
		"host: 127.0.0.1",
		"# loopback only",
		"# === auth ===",
		"enabled: false",
		"level: info",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected preserved content %q missing from:\n%s", want, s)
		}
	}

	// server appears before tools (key order preserved).
	if strings.Index(s, "server:") > strings.Index(s, "tools:") {
		t.Errorf("key order not preserved (server should precede tools):\n%s", s)
	}

	// Round-trip: the written values read back correctly.
	pc, err := ReadPermissionConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if pc.DefaultPermission != "deny_permanent" {
		t.Errorf("default_permission = %q, want deny_permanent", pc.DefaultPermission)
	}
	if len(pc.PresetRules) != 2 || pc.PresetRules[0].Tool != "Read" || pc.PresetRules[1].Pattern != "npm *" {
		t.Errorf("preset_rules round-trip mismatch: %+v", pc.PresetRules)
	}

	// A sibling key inside tools (default_timeout_seconds) is preserved.
	if !strings.Contains(s, "default_timeout_seconds: 60") {
		t.Errorf("sibling tools key not preserved:\n%s", s)
	}
}

func TestWritePermissionConfig_CreatesToolsWhenMissing(t *testing.T) {
	p := writeTemp(t, "server:\n  port: 7821\n")
	pc := PermissionConfig{
		DefaultPermission: "allow_permanent",
		PresetRules:       []PresetRule{{Tool: "*", Pattern: "*", Decision: "allow_permanent"}},
	}
	if err := WritePermissionConfig(p, pc); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPermissionConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultPermission != "allow_permanent" || len(got.PresetRules) != 1 {
		t.Errorf("create-tools round-trip mismatch: %+v", got)
	}
}
