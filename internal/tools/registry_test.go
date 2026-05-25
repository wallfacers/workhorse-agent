package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

type fakeTool struct{ name string }

func (f fakeTool) Name() string                  { return f.name }
func (f fakeTool) Description() string           { return "" }
func (f fakeTool) InputSchema() json.RawMessage  { return []byte(`{}`) }
func (f fakeTool) IsReadOnly() bool              { return true }
func (f fakeTool) CanRunInParallel() bool        { return true }
func (f fakeTool) DefaultTimeout() time.Duration { return 0 }
func (f fakeTool) Run(context.Context, *tools.Env, json.RawMessage) (*tools.Result, error) {
	return &tools.Result{Output: f.name}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := tools.NewRegistry()
	if err := r.Register(fakeTool{name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fakeTool{name: "B"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("A"); !ok {
		t.Error("Get(A) missing")
	}
	if _, ok := r.Get("C"); ok {
		t.Error("Get(C) should be missing")
	}
}

func TestRegistry_RejectsDuplicates(t *testing.T) {
	r := tools.NewRegistry()
	_ = r.Register(fakeTool{name: "A"})
	if err := r.Register(fakeTool{name: "A"}); err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestRegistry_Filtered(t *testing.T) {
	r := tools.NewRegistry()
	for _, n := range []string{"A", "B", "C"} {
		_ = r.Register(fakeTool{name: n})
	}
	// nil allowed → all tools
	if got := r.Filtered(nil); len(got) != 3 {
		t.Errorf("unfiltered: got %d, want 3", len(got))
	}
	// allow list
	got := r.Filtered([]string{"A", "C"})
	if len(got) != 2 || got[0].Name() != "A" || got[1].Name() != "C" {
		t.Errorf("filtered: %v", names(got))
	}
}

func TestRegistry_Names_Deterministic(t *testing.T) {
	r := tools.NewRegistry()
	for _, n := range []string{"Z", "A", "M"} {
		_ = r.Register(fakeTool{name: n})
	}
	got := r.Names()
	want := []string{"A", "M", "Z"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func names(ts []tools.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
