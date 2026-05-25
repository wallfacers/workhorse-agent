// Package agent owns the orchestration glue between the LLM stream, the tool
// registry, and the session state machine. The orchestrator here covers the
// per-turn tool execution: batching by CanRunInParallel, running concurrent
// batches under a semaphore, applying ContextModifiers after each batch.
//
// Higher-level loop logic (LLM call → tool calls → re-call) lives in
// agent/loop.go in Group 8.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// ToolCall is one tool_use the LLM emitted in the current turn.
type ToolCall struct {
	ID    string // tool_use_id, opaque per provider
	Name  string // matches Tool.Name()
	Input json.RawMessage
}

// ToolBatch is a slice of ToolCall the orchestrator runs together. Parallel
// batches run concurrent (subject to MaxParallel); serial batches run one
// call at a time. Per spec, each non-parallel tool sits in its own batch so
// serial-vs-serial ordering is preserved.
type ToolBatch struct {
	Parallel bool
	Calls    []ToolCall
}

// ToolCallResult is the outcome of one ToolCall. Result is always populated
// (even on failures — failures become IsError=true tool results) so the
// agent loop can feed them back to the LLM uniformly.
type ToolCallResult struct {
	ID     string
	Name   string
	Result *tools.Result
}

// Orchestrator wires the registry to the rest of the system. Construct one
// per process; safe for concurrent calls.
type Orchestrator struct {
	Registry       *tools.Registry
	MaxParallel    int
	DefaultTimeout time.Duration
	// PerToolTimeouts maps tool name to a config-supplied override. Optional;
	// tool.DefaultTimeout() still wins when non-zero.
	PerToolTimeouts map[string]time.Duration
	// MaxResultBytes caps tool output via TruncateOutput. Zero disables truncation.
	MaxResultBytes int
}

// BatchTools walks the call slice and produces ToolBatches:
//
//   - consecutive parallel-eligible tools collapse into one parallel batch
//   - every non-parallel tool sits in its own serial batch
//
// The ordering between batches is preserved so a serial call between two
// parallel groups acts as a barrier.
func BatchTools(reg *tools.Registry, calls []ToolCall) []ToolBatch {
	if len(calls) == 0 {
		return nil
	}
	var out []ToolBatch
	var par []ToolCall
	flushPar := func() {
		if len(par) > 0 {
			out = append(out, ToolBatch{Parallel: true, Calls: par})
			par = nil
		}
	}
	for _, c := range calls {
		t, ok := reg.Get(c.Name)
		if ok && t.CanRunInParallel() {
			par = append(par, c)
			continue
		}
		flushPar()
		out = append(out, ToolBatch{Parallel: false, Calls: []ToolCall{c}})
	}
	flushPar()
	return out
}

// RunBatch executes a single ToolBatch and returns one ToolCallResult per
// call, in input order. For parallel batches, MaxParallel bounds simultaneous
// runners via a weighted semaphore. A single tool's failure does NOT cancel
// siblings — only ctx cancel does.
func (o *Orchestrator) RunBatch(ctx context.Context, env *tools.Env, b ToolBatch) []ToolCallResult {
	results := make([]ToolCallResult, len(b.Calls))
	if !b.Parallel {
		for i, call := range b.Calls {
			results[i] = o.runOne(ctx, env, call)
		}
		return results
	}

	maxN := o.MaxParallel
	if maxN <= 0 {
		maxN = 10
	}
	sem := semaphore.NewWeighted(int64(maxN))
	var wg sync.WaitGroup
	wg.Add(len(b.Calls))
	for i, call := range b.Calls {
		i, call := i, call
		go func() {
			defer wg.Done()
			if err := sem.Acquire(ctx, 1); err != nil {
				// ctx canceled before we got a slot.
				results[i] = ToolCallResult{
					ID:   call.ID,
					Name: call.Name,
					Result: &tools.Result{
						Output:  fmt.Sprintf("tool canceled: %s", err),
						IsError: true,
					},
				}
				return
			}
			defer sem.Release(1)
			results[i] = o.runOne(ctx, env, call)
		}()
	}
	wg.Wait()
	return results
}

// RunAll batches calls and runs each batch in sequence. After every batch
// completes, the orchestrator applies any ContextModifier from successful
// (non-error) results in the order they appeared — that's the "deferred
// modifier" rule from spec: a same-batch sibling never sees a half-applied
// AllowedTools change.
func (o *Orchestrator) RunAll(ctx context.Context, env *tools.Env, target tools.ModifierTarget, calls []ToolCall) []ToolCallResult {
	batches := BatchTools(o.Registry, calls)
	var all []ToolCallResult
	for _, b := range batches {
		results := o.RunBatch(ctx, env, b)
		all = append(all, results...)
		for _, r := range results {
			if r.Result == nil || r.Result.IsError || r.Result.Modifier == nil {
				continue
			}
			if target == nil {
				continue
			}
			if err := r.Result.Modifier.Apply(target); err != nil {
				// Modifier failure is logged via env.Logger when available;
				// the call result already reached the model so we don't
				// retroactively flip is_error.
				if env != nil && env.Logger != nil {
					env.Logger.Warn("modifier apply failed",
						"tool", r.Name, "err", err.Error())
				}
			}
		}
	}
	return all
}

// runOne dispatches a single tool call. It always returns a result with a
// non-nil Result so callers don't have to defensive-check; on unrecoverable
// failures (panic, unknown tool, ctx cancel) the result has IsError=true.
func (o *Orchestrator) runOne(ctx context.Context, env *tools.Env, call ToolCall) (out ToolCallResult) {
	out = ToolCallResult{ID: call.ID, Name: call.Name}

	tool, ok := o.Registry.Get(call.Name)
	if !ok {
		out.Result = &tools.Result{
			Output:  fmt.Sprintf("unknown tool: %s", call.Name),
			IsError: true,
		}
		return
	}

	timeout := o.resolveTimeout(tool)
	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	defer func() {
		if r := recover(); r != nil {
			out.Result = &tools.Result{
				Output:  fmt.Sprintf("tool panicked: %v", r),
				IsError: true,
			}
		}
	}()

	res, err := tool.Run(runCtx, env, call.Input)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			out.Result = &tools.Result{
				Output:  fmt.Sprintf("tool execution timed out after %s", timeout),
				IsError: true,
			}
		case errors.Is(err, context.Canceled):
			out.Result = &tools.Result{
				Output:  "tool canceled",
				IsError: true,
			}
		default:
			out.Result = &tools.Result{
				Output:  err.Error(),
				IsError: true,
			}
		}
		return
	}
	if res == nil {
		out.Result = &tools.Result{Output: "(no output)", IsError: false}
		return
	}
	if truncated, ok := tools.TruncateOutput(res.Output, o.MaxResultBytes); ok {
		res.Output = truncated
	}
	out.Result = res
	return
}

// resolveTimeout follows the spec priority chain:
//
//  1. Tool.DefaultTimeout() — implementations supply their natural cap
//  2. PerToolTimeouts[name] — config.tools.<name>.timeout_seconds
//  3. Orchestrator.DefaultTimeout — config.tools.default_timeout_seconds
func (o *Orchestrator) resolveTimeout(t tools.Tool) time.Duration {
	if d := t.DefaultTimeout(); d > 0 {
		return d
	}
	if d, ok := o.PerToolTimeouts[t.Name()]; ok && d > 0 {
		return d
	}
	if o.DefaultTimeout > 0 {
		return o.DefaultTimeout
	}
	return 120 * time.Second
}
