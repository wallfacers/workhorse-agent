package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_Version(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"version"}, &stdout, io.Discard); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stdout.String(), "workhorse-agent") {
		t.Errorf("expected version banner, got %q", stdout.String())
	}
}

func TestRun_NoArgs_PrintsUsageAndExits2(t *testing.T) {
	var stderr bytes.Buffer
	err := run(nil, io.Discard, &stderr)
	if !errors.Is(err, errExitUsage) {
		t.Fatalf("expected errExitUsage, got %v", err)
	}
	if !strings.Contains(stderr.String(), "USAGE") {
		t.Errorf("usage text not emitted: %q", stderr.String())
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	err := run([]string{"floof"}, io.Discard, &stderr)
	if !errors.Is(err, errExitUsage) {
		t.Fatalf("expected errExitUsage, got %v", err)
	}
	if !strings.Contains(stderr.String(), `unknown command "floof"`) {
		t.Errorf("unknown-command stderr missing offending name: %q", stderr.String())
	}
}

func TestExtractConfigPath(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--config", "/etc/da.yaml"}, "/etc/da.yaml"},
		{[]string{"--config=/etc/da.yaml"}, "/etc/da.yaml"},
		{[]string{"--port", "9000"}, ""},
		{[]string{}, ""},
	}
	for _, c := range cases {
		if got := extractConfigPath(c.args); got != c.want {
			t.Errorf("extractConfigPath(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestRunInit_CreatesAllArtifacts(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	answers := strings.NewReader("anthropic\nsk-test\n7821\nN\n")
	stdin := os.Stdin
	defer func() { os.Stdin = stdin }()

	// swap os.Stdin with a temp file that holds the canned answers
	stdinPipe := writeStdinFile(t, answers)
	os.Stdin = stdinPipe
	defer stdinPipe.Close() //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if err := runInit(nil, &stdout, &stderr); err != nil {
		t.Fatalf("runInit: %v (stderr=%q)", err, stderr.String())
	}

	root := filepath.Join(tmpHome, ".workhorse-agent")
	for _, child := range []string{"config.yaml", "mcp.json", "state.db", "skills", "agents"} {
		p := filepath.Join(root, child)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist, got %v", p, err)
		}
	}

	cfgBytes, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(cfgBytes)
	if !strings.Contains(cfg, "default: anthropic") {
		t.Errorf("config should record provider choice, got:\n%s", cfg)
	}
	if !strings.Contains(cfg, "api_key: \"sk-test\"") {
		t.Errorf("config should record api key, got:\n%s", cfg)
	}
	if strings.Contains(cfg, "enabled: true") {
		t.Errorf("auth should be disabled, got:\n%s", cfg)
	}
}

func TestRunInit_RefusesToOverwrite(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	root := filepath.Join(tmpHome, ".workhorse-agent")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("# pre-existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	err := runInit(nil, io.Discard, &stderr)
	if !errors.Is(err, errExitUsage) {
		t.Fatalf("expected errExitUsage when config.yaml exists, got %v", err)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr should explain why init refused, got %q", stderr.String())
	}
}

func writeStdinFile(t *testing.T, r io.Reader) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return f
}
