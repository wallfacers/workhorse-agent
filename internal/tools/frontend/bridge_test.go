package frontend

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

func TestCall_ResolveOK(t *testing.T) {
	var emitted atomic.Pointer[map[string]any]
	emit := func(_ context.Context, _ string, payload map[string]any) error {
		emitted.Store(&payload)
		return nil
	}
	b := NewBridge(emit)

	resultCh := make(chan *tools.Result, 1)
	go func() {
		res, err := b.Call(context.Background(), "click", json.RawMessage(`{"selector":"#btn"}`))
		if err != nil {
			resultCh <- &tools.Result{Output: err.Error(), IsError: true}
			return
		}
		resultCh <- res
	}()

	// Wait for emit to fire.
	time.Sleep(20 * time.Millisecond)

	ev := *emitted.Load()
	id, _ := ev["tool_use_id"].(string)
	if id == "" {
		t.Fatal("expected tool_use_id in emitted event")
	}
	if ev["name"] != "click" {
		t.Fatalf("expected name=click, got %v", ev["name"])
	}

	b.Resolve(id, json.RawMessage(`{"ok":true,"value":{"clicked":true}}`))

	res := <-resultCh
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	var val map[string]bool
	if err := json.Unmarshal([]byte(res.Output), &val); err != nil {
		t.Fatal(err)
	}
	if !val["clicked"] {
		t.Fatal("expected clicked=true")
	}
}

func TestCall_ResolveError(t *testing.T) {
	var emitted atomic.Pointer[map[string]any]
	emit := func(_ context.Context, _ string, payload map[string]any) error {
		emitted.Store(&payload)
		return nil
	}
	b := NewBridge(emit)

	resultCh := make(chan *tools.Result, 1)
	go func() {
		res, err := b.Call(context.Background(), "click", json.RawMessage(`{}`))
		if err != nil {
			resultCh <- &tools.Result{Output: err.Error(), IsError: true}
			return
		}
		resultCh <- res
	}()

	time.Sleep(20 * time.Millisecond)
	ev := *emitted.Load()
	id, _ := ev["tool_use_id"].(string)

	b.Resolve(id, json.RawMessage(`{"ok":false,"error":{"kind":"not_found","message":"element not found"}}`))

	res := <-resultCh
	if !res.IsError {
		t.Fatal("expected IsError=true")
	}
	if res.Output != "element not found" {
		t.Fatalf("unexpected output: %s", res.Output)
	}
}

func TestCall_CancelCleanup(t *testing.T) {
	emit := func(_ context.Context, _ string, _ map[string]any) error {
		return nil
	}
	b := NewBridge(emit)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Call(ctx, "click", json.RawMessage(`{}`))
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	b.mu.Lock()
	count := len(b.pending)
	b.mu.Unlock()
	if count != 0 {
		t.Fatalf("expected 0 pending entries after cancel, got %d", count)
	}
}

func TestResolve_UnknownID(t *testing.T) {
	emit := func(_ context.Context, _ string, _ map[string]any) error { return nil }
	b := NewBridge(emit)

	// Should not panic on unknown id.
	b.Resolve("nonexistent", json.RawMessage(`{"ok":true,"value":null}`))
}

func TestTool_ParallelSafety(t *testing.T) {
	emit := func(_ context.Context, _ string, _ map[string]any) error { return nil }
	b := NewBridge(emit)

	safeTool := NewTool(ToolSpec{Name: "safe_tool", ParallelSafety: "safe"}, b)
	if !safeTool.CanRunInParallel() {
		t.Error("expected CanRunInParallel=true for safe tool")
	}
	if safeTool.IsReadOnly() {
		t.Error("IsReadOnly should always be false for frontend tools")
	}

	unsafeTool := NewTool(ToolSpec{Name: "unsafe_tool", ParallelSafety: "unsafe"}, b)
	if unsafeTool.CanRunInParallel() {
		t.Error("expected CanRunInParallel=false for unsafe tool")
	}

	defaultTool := NewTool(ToolSpec{Name: "default_tool"}, b)
	if defaultTool.CanRunInParallel() {
		t.Error("expected CanRunInParallel=false when parallelSafety is empty")
	}
}

func TestTool_DefaultTimeout(t *testing.T) {
	emit := func(_ context.Context, _ string, _ map[string]any) error { return nil }
	b := NewBridge(emit)
	tool := NewTool(ToolSpec{Name: "t"}, b)
	if tool.DefaultTimeout() != 0 {
		t.Error("expected DefaultTimeout=0 (inherit config)")
	}
}
