# Real E2E Testing: LLM-as-Judge + Record/Replay

**Date**: 2026-05-30
**Status**: Draft
**Scope**: `test/real_e2e/` (new directory, no changes to existing `test/e2e/`)

## Problem

workhorse-agent has 15+ E2E tests using `mockprovider` (scripted LLM responses). These
validate protocol compliance, auth, SSE streaming, and session mechanics — but they never
touch a real LLM. We have no automated way to verify that the full chain
(user message → DashScope API → agent loop → tool dispatch → response) actually works
end-to-end.

The challenge: LLM outputs are non-deterministic. Traditional string-assertion tests are
brittle against model output variation. We need a testing strategy that:

1. **Covers real provider integration** (DashScope qwen3.6-plus via Anthropic-compatible API)
2. **Handles non-determinism** (cross-model evaluation instead of string matching)
3. **Controls cost and latency** (recorded sessions for CI, real API for release validation)
4. **Is granularly selectable** (run smoke tests in 30s, or full suite in 10min)

## Design

### Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│                     test/real_e2e/                       │
│                                                          │
│  ┌──────────┐    ┌──────────────┐    ┌───────────────┐  │
│  │ Test      │───▶│ Recording    │───▶│ DashScope     │  │
│  │ Runner    │    │ Provider     │    │ qwen3.6-plus  │  │
│  │           │    │ (decorator)  │    │ (real API)    │  │
│  └──────────┘    └──────────────┘    └───────────────┘  │
│       │                                       │          │
│       │           ┌──────────────┐            │          │
│       └──────────▶│ Trace        │◀───────────┘          │
│                   │ Collector    │  (interaction trace)   │
│                   └──────┬───────┘                       │
│                          │                                │
│                   ┌──────▼───────┐    ┌───────────────┐  │
│                   │ LLM Judge    │───▶│ DashScope     │  │
│                   │ (GLM-5)      │    │ glm-5         │  │
│                   └──────────────┘    └───────────────┘  │
│                          │                                │
│                   ┌──────▼───────┐                       │
│                   │ Verdict:     │                       │
│                   │ PASS / FAIL  │                       │
│                   └──────────────┘                       │
└─────────────────────────────────────────────────────────┘
```

Three independent axes of control:

| Axis | Env Var | Values | Default |
|------|---------|--------|---------|
| Provider mode | `WORKHORSE_TEST_MODE` | `replay` / `record` / `live` / `skip` | `replay` |
| Judge mode | `WORKHORSE_JUDGE_MODE` | `llm` / `cached` / `off` | `cached` |
| Test level | Go build tags | `smoke` / `integration` / `full` | `smoke` |

### Component 1: Recording Provider (`recorder.go`)

A decorator wrapping `provider.Provider`. Implements the same `Provider` interface so it
drops into the existing `agent.Loop` without changes.

```go
type Mode int

const (
    ModeReplay Mode = iota  // read from JSONL fixtures
    ModeRecord               // call real API + write JSONL
    ModeLive                 // call real API, no recording
)

type RecordingProvider struct {
    inner   provider.Provider
    mode    Mode
    dir     string  // fixture directory for this test
    testID  string  // unique per test case
}
```

**Record mode**: `Stream()` calls `inner.Stream()`, collects all events, then serializes
`provider.Request` + `[]provider.ProviderEvent` to `test/fixtures/recordings/<testID>.jsonl`.
One JSON line per Stream call (one agent turn).

**Replay mode**: `Stream()` reads the next JSONL line, deserializes events, pushes them
through a channel — identical semantics to `mockprovider` but loaded from disk.

**Live mode**: pure passthrough, no recording.

**Skip mode**: if no API key is configured, tests call `t.Skip("no API key")`.

Key design decisions:
- JSONL (one line per turn) over a single JSON file: easier to append during recording,
  easier to debug by reading individual lines.
- Recordings are committed to git: team-shared, CI-stable, version-controlled.
- A recording header stores model name + timestamp for drift detection.

### Component 2: LLM-as-Judge (`judge.go`)

```go
type Verdict string

const (
    VerdictPass    Verdict = "PASS"
    VerdictFail    Verdict = "FAIL"
    VerdictPartial Verdict = "PARTIAL"
)

type JudgeResult struct {
    Verdict     Verdict   `json:"verdict"`
    Score       float64   `json:"score"`       // 0.0 - 1.0
    Reasoning   string    `json:"reasoning"`
    Suggestions []string  `json:"suggestions"`
}

type Rubric struct {
    Criteria   []Criterion
    MinScore   float64  // minimum to consider PASS
    MaxRetries int      // retry on PARTIAL (LLM non-determinism)
}

type Criterion struct {
    Name        string
    Description string
    Weight      float64
    Required    bool  // if any Required criterion fails, overall FAIL
}

type Judge interface {
    Evaluate(ctx context.Context, trace Trace, rubric Rubric) (*JudgeResult, error)
}
```

**Judge implementation** (`glm5.go`):
- Constructs a prompt containing: the original user message, the full interaction trace
  (tool calls, tool results, model responses), and the rubric criteria.
- Calls GLM-5 via DashScope's Anthropic-compatible API (`/apps/anthropic`).
- Requests a structured JSON response containing verdict, score, reasoning.
- Parses and validates the JSON response.

**Judge caching**:
- Judge results are cached in `test/fixtures/judge_cache/<testID>_<hash>.json`.
- Hash is over (trace content + rubric JSON). Same trace + same rubric = cached result.
- `WORKHORSE_JUDGE_MODE=cached` reads from cache only.
- `WORKHORSE_JUDGE_MODE=llm` always calls real GLM-5, updates cache.

**Retry logic**:
- On `PARTIAL`, the test retries up to `MaxRetries` times with a fresh LLM call.
- This handles the inherent non-determinism: if the model was "almost right", a retry
  may push it over the threshold.
- Each retry re-sends the same user prompt (no mutation of test input).

### Component 3: Trace Collector (`trace.go`)

```go
type Trace struct {
    TestName    string
    UserMessage string
    Turns      []Turn
}

type Turn struct {
    ModelOutput    string           // accumulated text from model
    ToolCalls      []ToolCallRecord // tool_use blocks from model
    ToolResults    []ToolResultRecord
    Duration       time.Duration
}

type ToolCallRecord struct {
    ToolName string
    Input    json.RawMessage
}

type ToolResultRecord struct {
    ToolName string
    Output   string
    IsError  bool
}
```

The trace collector hooks into the agent loop's existing event channel. It records:
- All `EventTextDelta` → accumulated into `ModelOutput`
- All `EventToolUse` → captured as `ToolCallRecord`
- Tool results from the orchestrator → captured as `ToolResultRecord`

This trace is what gets sent to the Judge — not the raw SSE events.

### Component 4: Test Runner Helpers (`helpers.go`)

```go
// newRealStack builds a full test stack with RecordingProvider.
// Reads WORKHORSE_TEST_MODE to decide provider mode.
// Calls t.Skip() if mode requires an API key but none is configured.
func newRealStack(t *testing.T) *realStack

// assertVerdict runs the Judge and asserts PASS.
// Respects WORKHORSE_JUDGE_MODE for judge behavior.
// On failure, prints Judge's reasoning and suggestions.
func assertVerdict(t *testing.T, trace Trace, rubric Rubric)

// runScenario is the main test driver:
// 1. Start test server with RecordingProvider
// 2. Send user message via HTTP API
// 3. Collect SSE events into Trace
// 4. Optionally run Judge
// 5. Assert verdict
func runScenario(t *testing.T, cfg ScenarioConfig) Trace
```

### Component 5: Test Scenarios

Three subsystems, organized by file:

**`file_tools_test.go`** — File operation tools:

| Test | Level | Description |
|------|-------|-------------|
| `TestFileRead_Basic` | smoke | Read a known file, model reports its content |
| `TestFileRead_NotFound` | smoke | Read nonexistent path, model reports error |
| `TestFileWrite_Create` | integration | Write a new file, then read to verify |
| `TestFileEdit_Modify` | integration | Edit existing file, verify changes |
| `TestBash_ListDir` | smoke | Run `ls` via Bash tool, model reports files |
| `TestGrep_Search` | integration | Search for a pattern, model reports matches |
| `TestMultiTool_Workflow` | full | Read → analyze → Edit pipeline |

**`memory_test.go`** — Memory subsystem:

| Test | Level | Description |
|------|-------|-------------|
| `TestMemoryWrite_Read` | smoke | Write memory, read back in same session |
| `TestMemoryCrossSession` | integration | Write in session A, visible in session B |
| `TestSessionSearch_Basic` | smoke | Store messages, search finds them |
| `TestSessionSearch_CJK` | integration | CJK trigram search works correctly |

**`extagent_test.go`** — External agents:

| Test | Level | Description |
|------|-------|-------------|
| `TestExtAgent_Invoke` | smoke | Invoke a known external agent |
| `TestExtAgent_Error` | integration | Handle failed agent execution gracefully |

Each test defines its own Rubric tailored to the scenario.

### Component 6: Rubric Definitions (`rubrics.go`)

Example rubrics for each test category:

**File tools rubric**:
```
- tool_call_correct (weight: 0.3, required):
    Did the model call the correct tool with correct parameters?
- response_accuracy (weight: 0.35, required):
    Does the response accurately reflect the tool result?
- no_hallucination (weight: 0.2, required):
    Did the model avoid fabricating information not in the tool result?
- efficiency (weight: 0.15):
    Did the model avoid unnecessary extra tool calls?
MinScore: 0.7, MaxRetries: 2
```

**Memory rubric**:
```
- tool_invocation (weight: 0.3, required):
    Did the model use memory_read/memory_write correctly?
- data_integrity (weight: 0.4, required):
    Is the read data identical to what was written?
- cross_session (weight: 0.3, required):
    Can session B see what session A wrote?
MinScore: 0.8, MaxRetries: 1
```

**External agent rubric**:
```
- correct_invocation (weight: 0.4, required):
    Did the model invoke the right agent with the right arguments?
- output_handling (weight: 0.3, required):
    Did the model correctly process and relay the agent's output?
- error_recovery (weight: 0.3):
    On failure, did the model provide a useful explanation?
MinScore: 0.7, MaxRetries: 2
```

### Execution Modes

**CI (default, zero cost)**:
```bash
WORKHORSE_TEST_MODE=replay WORKHORSE_JUDGE_MODE=cached \
  go test ./test/real_e2e/... -tags=smoke -timeout 5m
```

**Pre-release validation (costs tokens)**:
```bash
WORKHORSE_TEST_MODE=live WORKHORSE_JUDGE_MODE=llm \
  go test ./test/real_e2e/... -tags=full -timeout 15m
```

**Update recordings (costs tokens)**:
```bash
WORKHORSE_TEST_MODE=record WORKHORSE_JUDGE_MODE=off \
  go test ./test/real_e2e/... -run TestFileRead_Basic -timeout 5m
```

**Quick manual check (costs tokens)**:
```bash
WORKHORSE_TEST_MODE=live WORKHORSE_JUDGE_MODE=off \
  go test ./test/real_e2e/... -run TestFileRead_Basic -timeout 2m -v
```

### Recording File Format

```jsonl
{"header":{"test":"TestFileRead_Basic","model":"qwen3.6-plus","recorded_at":"2026-05-30T10:00:00Z"}}
{"request":{"model":"qwen3.6-plus","system":"...","messages":[...],"tools":[...],"max_tokens":4096},"events":[{"type":"text_delta","text_delta":"Let me"},{"type":"text_delta","text_delta":" read that"},{"type":"tool_use","tool_use":{"tool_name":"Read","input":"{\"file_path\":\"/tmp/test.txt\"}"}},"type":"stop","stop_reason":"tool_use"},{"type":"text_delta","text_delta":"The file contains"},{"type":"stop","stop_reason":"end_turn"}]}
```

- Line 1: header with test name, model, timestamp.
- Subsequent lines: one per `Stream()` call, containing serialized `Request` + `[]ProviderEvent`.

### Build Tag Strategy

Tests use Go's conventional build tags for level selection:

- `//go:build real_e2e` — all real E2E tests require this tag to avoid accidental runs
- Inside real E2E tests, level filtering uses `-run` with naming conventions:
  - `Test*_Smoke*` — smoke level
  - `Test*_Integration*` — integration level
  - `Test*_Full*` — full scenario

This keeps real E2E tests completely separated from unit and mock E2E tests. Running
`go test ./...` without `-tags=real_e2e` skips the entire `test/real_e2e/` tree.

### Timeout and Retry Strategy

- Each test has a per-scenario timeout (default: 60s for smoke, 180s for integration,
  300s for full).
- The RecordingProvider enforces a stream-level timeout: if no event arrives within 30s,
  the stream is canceled.
- Judge calls have a 45s timeout.
- On `VerdictPartial`, retry the entire scenario (fresh session, same input) up to
  `MaxRetries`. Do NOT retry individual LLM calls — the non-determinism is at the
  scenario level.

### Recording Drift Detection

When running in `replay` mode, compare the current model name in config against the
model name stored in the recording header. If they differ, log a warning:

```
Recording was made with "qwen3.6-plus" but current model is "qwen4.0-plus".
Consider re-recording with WORKHORSE_TEST_MODE=record.
```

This does NOT fail the test — recordings are still valid for protocol-level validation.
But it signals that the Judge evaluations may not reflect current model behavior.

### Relationship to Existing Tests

```
test/e2e/          ← existing, untouched
  e2e_test.go        mockprovider-based protocol tests
  extagent_test.go   mockprovider-based ext agent tests
  adapter_generator_test.go

test/real_e2e/     ← new, independent
  judge/             LLM-as-Judge infrastructure
  tests/             scenario tests with real LLM
  fixtures/          recordings + judge cache
```

No shared state, no shared test helpers (beyond what's in `test/real_e2e/` itself).
The two directories can be run independently.

### Cost Estimate

| Mode | Tokens per test (approx) | Cost per test (qwen3.6-plus) | Cost per test (GLM-5 judge) |
|------|-------------------------|-------------------------------|------------------------------|
| smoke | 500-2000 | ~$0.001 | ~$0.002 |
| integration | 2000-8000 | ~$0.005 | ~$0.005 |
| full | 5000-20000 | ~$0.01 | ~$0.01 |
| **Full suite (~15 tests)** | — | ~$0.10 | ~$0.10 |

Replay + cached mode: **$0.00**.
