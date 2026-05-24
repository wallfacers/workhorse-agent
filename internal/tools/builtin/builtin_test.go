package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/data-agent/internal/tools"
	"github.com/wallfacers/data-agent/internal/tools/builtin"
)

func env(t *testing.T) *tools.Env {
	t.Helper()
	return &tools.Env{Workdir: t.TempDir()}
}

func mustWrite(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- Read ----

func TestRead_Happy(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.txt", "hello\nworld\n")
	res, _ := builtin.Read{}.Run(context.Background(), e, json.RawMessage(`{"path":"a.txt"}`))
	if res.IsError {
		t.Fatalf("Read failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") || !strings.Contains(res.Output, "world") {
		t.Errorf("content missing: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1\t") || !strings.Contains(res.Output, "2\t") {
		t.Errorf("line numbers missing: %s", res.Output)
	}
}

func TestRead_RejectsPathEscape(t *testing.T) {
	e := env(t)
	res, _ := builtin.Read{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"../../etc/passwd"}`))
	if !res.IsError {
		t.Error("expected error for path escape")
	}
}

func TestRead_OffsetLimit(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "f.txt", "L1\nL2\nL3\nL4\nL5\n")
	res, _ := builtin.Read{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"f.txt","offset":2,"limit":2}`))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if strings.Contains(res.Output, "L1") || !strings.Contains(res.Output, "L2") || !strings.Contains(res.Output, "L3") || strings.Contains(res.Output, "L4") {
		t.Errorf("offset/limit window wrong: %s", res.Output)
	}
}

// ---- Write ----

func TestWrite_Atomic(t *testing.T) {
	e := env(t)
	res, _ := builtin.Write{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"new.txt","content":"hi"}`))
	if res.IsError {
		t.Fatal(res.Output)
	}
	body, err := os.ReadFile(filepath.Join(e.Workdir, "new.txt"))
	if err != nil || string(body) != "hi" {
		t.Errorf("write failed: body=%q err=%v", body, err)
	}
}

func TestWrite_RejectsEscape(t *testing.T) {
	e := env(t)
	res, _ := builtin.Write{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"../escape.txt","content":"hi"}`))
	if !res.IsError {
		t.Error("expected error for write escape")
	}
}

// ---- Edit ----

func TestEdit_Happy(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.txt", "hello world\n")
	res, _ := builtin.Edit{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"a.txt","old_string":"world","new_string":"go"}`))
	if res.IsError {
		t.Fatal(res.Output)
	}
	body, _ := os.ReadFile(filepath.Join(e.Workdir, "a.txt"))
	if string(body) != "hello go\n" {
		t.Errorf("edit body: %q", body)
	}
}

func TestEdit_NotFound(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.txt", "hello\n")
	res, _ := builtin.Edit{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"a.txt","old_string":"goodbye","new_string":"x"}`))
	if !res.IsError || !strings.Contains(res.Output, "not found") {
		t.Errorf("expected not-found error, got %+v", res)
	}
}

func TestEdit_MultipleWithoutReplaceAll(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.txt", "foo bar foo")
	res, _ := builtin.Edit{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"a.txt","old_string":"foo","new_string":"X"}`))
	if !res.IsError || !strings.Contains(res.Output, "replace_all=true") {
		t.Errorf("expected replace_all hint, got %s", res.Output)
	}
}

func TestEdit_ReplaceAll(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.txt", "foo bar foo")
	res, _ := builtin.Edit{}.Run(context.Background(), e,
		json.RawMessage(`{"path":"a.txt","old_string":"foo","new_string":"X","replace_all":true}`))
	if res.IsError {
		t.Fatal(res.Output)
	}
	body, _ := os.ReadFile(filepath.Join(e.Workdir, "a.txt"))
	if string(body) != "X bar X" {
		t.Errorf("body: %q", body)
	}
}

// ---- Grep ----

func TestGrep_FindsMatches(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "main.go", "package main\nfunc Foo() {}\nfunc Bar() {}\n")
	mustWrite(t, e.Workdir, "util.go", "func Baz() {}\nfunc Foo() {}\n")
	res, _ := builtin.Grep{}.Run(context.Background(), e,
		json.RawMessage(`{"pattern":"func Foo"}`))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "main.go:2") {
		t.Errorf("missing main.go match: %s", res.Output)
	}
	if !strings.Contains(res.Output, "util.go:2") {
		t.Errorf("missing util.go match: %s", res.Output)
	}
}

func TestGrep_IncludeFilter(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.go", "// TODO refactor\n")
	mustWrite(t, e.Workdir, "a.md", "TODO write docs\n")
	res, _ := builtin.Grep{}.Run(context.Background(), e,
		json.RawMessage(`{"pattern":"TODO","include":"*.go"}`))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "a.go") {
		t.Errorf("missing a.go: %s", res.Output)
	}
	if strings.Contains(res.Output, "a.md") {
		t.Errorf("include filter ignored, a.md leaked: %s", res.Output)
	}
}

func TestGrep_NoMatches(t *testing.T) {
	e := env(t)
	mustWrite(t, e.Workdir, "a.go", "package main\n")
	res, _ := builtin.Grep{}.Run(context.Background(), e,
		json.RawMessage(`{"pattern":"zzznotfoundzzz"}`))
	if res.IsError || !strings.Contains(res.Output, "no matches") {
		t.Errorf("expected (no matches), got %+v", res)
	}
}
