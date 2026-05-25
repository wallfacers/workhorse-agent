package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// ---- stub tools ----

type stubTool struct {
	name        string
	parallel    bool
	readOnly    bool
	timeout     time.Duration
	body        func(ctx context.Context) (*tools.Result, error)
	concurrency *int64 // atomic counter; max value records peak parallelism
	maxSeen     *int64
}

func (s *stubTool) Name() string                  { return s.name }
func (s *stubTool) Description() string           { return "" }
func (s *stubTool) InputSchema() json.RawMessage  { return []byte(`{}`) }
func (s *stubTool) IsReadOnly() bool              { return s.readOnly }
func (s *stubTool) CanRunInParallel() bool        { return s.parallel }
func (s *stubTool) DefaultTimeout() time.Duration { return s.timeout }
func (s *stubTool) Run(ctx context.Context, _ *tools.Env, _ json.RawMessage) (*tools.Result, error) {
	if s.concurrency != nil {
		cur := atomic.AddInt64(s.concurrency, 1)
		for {
			seen := atomic.LoadInt64(s.maxSeen)
			if cur <= seen {
				break
			}
			if atomic.CompareAndSwapInt64(s.maxSeen, seen, cur) {
				break
			}
		}
		defer atomic.AddInt64(s.concurrency, -1)
	}
	return s.body(ctx)
}

func makeReg(t *testing.T, ts ...*stubTool) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry()
	for _, s := range ts {
		if err := r.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// ---- batching ----

func TestBatchTools_MixedParallelSerial(t *testing.T) {
	A := &stubTool{name: "A", parallel: true}
	B := &stubTool{name: "B", parallel: true}
	C := &stubTool{name: "C", parallel: false}
	D := &stubTool{name: "D", parallel: true}
	r := makeReg(t, A, B, C, D)
	calls := []agent.ToolCall{
		{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"},
	}
	batches := agent.BatchTools(r, calls)
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3", len(batches))
	}
	if !batches[0].Parallel || len(batches[0].Calls) != 2 {
		t.Errorf("batch[0]: %+v", batches[0])
	}
	if batches[1].Parallel || len(batches[1].Calls) != 1 || batches[1].Calls[0].Name != "C" {
		t.Errorf("batch[1]: %+v", batches[1])
	}
	if !batches[2].Parallel || len(batches[2].Calls) != 1 || batches[2].Calls[0].Name != "D" {
		t.Errorf("batch[2]: %+v", batches[2])
	}
}

func TestBatchTools_AllSerial(t *testing.T) {
	A := &stubTool{name: "A", parallel: false}
	B := &stubTool{name: "B", parallel: false}
	r := makeReg(t, A, B)
	batches := agent.BatchTools(r, []agent.ToolCall{{Name: "A"}, {Name: "B"}})
	if len(batches) != 2 {
		t.Errorf("serial tools should each be their own batch, got %d", len(batches))
	}
}

// ---- parallel execution ----

func TestRunBatch_ParallelHonoursSemaphore(t *testing.T) {
	var live, peak int64
	body := func(ctx context.Context) (*tools.Result, error) {
		time.Sleep(50 * time.Millisecond)
		return &tools.Result{Output: "ok"}, nil
	}
	tools_ := make([]*stubTool, 5)
	for i := range tools_ {
		tools_[i] = &stubTool{
			name: nameOf(i), parallel: true, readOnly: true,
			body: body, concurrency: &live, maxSeen: &peak,
		}
	}
	r := makeReg(t, tools_...)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 2, DefaultTimeout: time.Second}

	calls := make([]agent.ToolCall, 5)
	for i := range calls {
		calls[i] = agent.ToolCall{ID: nameOf(i), Name: nameOf(i)}
	}
	results := o.RunBatch(context.Background(), &tools.Env{}, agent.ToolBatch{Parallel: true, Calls: calls})
	if len(results) != 5 {
		t.Fatalf("results: %d, want 5", len(results))
	}
	if peak > 2 {
		t.Errorf("MaxParallel=2 violated, peak=%d", peak)
	}
}

// ---- failure isolation ----

func TestRunBatch_FailingSiblingDoesNotCancelOthers(t *testing.T) {
	good := &stubTool{name: "good", parallel: true, body: func(context.Context) (*tools.Result, error) {
		return &tools.Result{Output: "ok"}, nil
	}}
	bad := &stubTool{name: "bad", parallel: true, body: func(context.Context) (*tools.Result, error) {
		return nil, errors.New("intentional fail")
	}}
	r := makeReg(t, good, bad)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 10, DefaultTimeout: time.Second}
	results := o.RunBatch(context.Background(), &tools.Env{},
		agent.ToolBatch{Parallel: true, Calls: []agent.ToolCall{
			{Name: "good"}, {Name: "bad"},
		}})
	if results[0].Result.IsError {
		t.Errorf("good should succeed, got %+v", results[0].Result)
	}
	if !results[1].Result.IsError {
		t.Errorf("bad should fail, got %+v", results[1].Result)
	}
}

// ---- timeout ----

func TestRunOne_AppliesTimeout(t *testing.T) {
	slow := &stubTool{name: "slow", parallel: true, timeout: 50 * time.Millisecond,
		body: func(ctx context.Context) (*tools.Result, error) {
			select {
			case <-time.After(time.Second):
				return &tools.Result{Output: "should not arrive"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	r := makeReg(t, slow)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 1}
	start := time.Now()
	results := o.RunBatch(context.Background(), &tools.Env{}, agent.ToolBatch{
		Parallel: false,
		Calls:    []agent.ToolCall{{Name: "slow"}},
	})
	if !results[0].Result.IsError || !contains(results[0].Result.Output, "timed out") {
		t.Errorf("expected timed-out marker, got %+v", results[0].Result)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("timeout escalation slow: %v", d)
	}
}

func TestRunOne_PanicBecomesError(t *testing.T) {
	boom := &stubTool{name: "boom", parallel: false, body: func(context.Context) (*tools.Result, error) {
		panic("kaboom")
	}}
	r := makeReg(t, boom)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 1, DefaultTimeout: time.Second}
	results := o.RunBatch(context.Background(), &tools.Env{}, agent.ToolBatch{
		Calls: []agent.ToolCall{{Name: "boom"}},
	})
	if !results[0].Result.IsError || !contains(results[0].Result.Output, "panicked") {
		t.Errorf("expected panic marker, got %+v", results[0].Result)
	}
}

// ---- modifier deferral ----

type allowedToolsRecorder struct{ allowed []string }

func (r *allowedToolsRecorder) SetAllowedTools(t []string) { r.allowed = t }

type setAllowedModifier struct{ list []string }

func (s setAllowedModifier) Apply(t tools.ModifierTarget) error {
	t.SetAllowedTools(s.list)
	return nil
}

func TestRunAll_ModifiersApplyAfterBatch(t *testing.T) {
	// Two parallel tools both want to set AllowedTools. They run together
	// (no sibling sees a half-applied list). Apply order is call order;
	// the last applier wins.
	first := &stubTool{name: "first", parallel: true, body: func(context.Context) (*tools.Result, error) {
		return &tools.Result{Modifier: setAllowedModifier{list: []string{"X"}}}, nil
	}}
	second := &stubTool{name: "second", parallel: true, body: func(context.Context) (*tools.Result, error) {
		return &tools.Result{Modifier: setAllowedModifier{list: []string{"Y"}}}, nil
	}}
	r := makeReg(t, first, second)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 5, DefaultTimeout: time.Second}
	rec := &allowedToolsRecorder{}
	o.RunAll(context.Background(), &tools.Env{}, rec, []agent.ToolCall{
		{Name: "first"}, {Name: "second"},
	})
	if len(rec.allowed) != 1 || rec.allowed[0] != "Y" {
		t.Errorf("modifier ordering broken, got %v", rec.allowed)
	}
}

func TestResolveTimeout_PriorityChain(t *testing.T) {
	// Run a no-op tool and verify deadline >= tool's DefaultTimeout (17s),
	// which the spec says wins over the orchestrator default + per-tool config.
	done := make(chan time.Duration, 1)
	captureTo := &stubTool{
		name: "y", parallel: true, timeout: 17 * time.Second,
		body: func(ctx context.Context) (*tools.Result, error) {
			d, _ := ctx.Deadline()
			done <- time.Until(d)
			return &tools.Result{Output: "ok"}, nil
		},
	}
	r2 := makeReg(t, captureTo)
	o2 := &agent.Orchestrator{Registry: r2, MaxParallel: 1, DefaultTimeout: 3 * time.Second}
	o2.RunBatch(context.Background(), &tools.Env{}, agent.ToolBatch{Calls: []agent.ToolCall{{Name: "y"}}})
	d := <-done
	if d < 16*time.Second {
		t.Errorf("deadline should reflect DefaultTimeout (17s), got %v", d)
	}
}

func TestRunOne_TruncatesLargeOutput(t *testing.T) {
	big := &stubTool{name: "big", parallel: false, body: func(context.Context) (*tools.Result, error) {
		return &tools.Result{Output: strings.Repeat("x", 10000)}, nil
	}}
	r := makeReg(t, big)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 1, MaxResultBytes: 1024}
	results := o.RunBatch(context.Background(), &tools.Env{}, agent.ToolBatch{
		Calls: []agent.ToolCall{{Name: "big"}},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Result.IsError {
		t.Fatalf("unexpected error: %s", results[0].Result.Output)
	}
	if len(results[0].Result.Output) > 1024 {
		t.Errorf("output length %d exceeds MaxResultBytes 1024", len(results[0].Result.Output))
	}
	if !strings.Contains(results[0].Result.Output, "[truncated:") {
		t.Errorf("expected truncation marker in output")
	}
}

func TestRunOne_NoTruncateWhenZero(t *testing.T) {
	big := &stubTool{name: "big2", parallel: false, body: func(context.Context) (*tools.Result, error) {
		return &tools.Result{Output: strings.Repeat("y", 5000)}, nil
	}}
	r := makeReg(t, big)
	o := &agent.Orchestrator{Registry: r, MaxParallel: 1, MaxResultBytes: 0}
	results := o.RunBatch(context.Background(), &tools.Env{}, agent.ToolBatch{
		Calls: []agent.ToolCall{{Name: "big2"}},
	})
	if len(results[0].Result.Output) != 5000 {
		t.Errorf("expected full output (5000 bytes), got %d", len(results[0].Result.Output))
	}
}

// helpers ----

func nameOf(i int) string { return string(rune('A' + i)) }
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && indexOf(s, sub) >= 0))
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
