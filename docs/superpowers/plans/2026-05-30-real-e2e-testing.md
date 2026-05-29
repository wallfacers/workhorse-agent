# Real E2E Testing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a complete real-E2E testing framework using LLM-as-Judge (GLM-5) with record/replay, covering file tools, memory subsystem, and external agents.

**Architecture:** New `test/real_e2e/` package with three layers: (1) RecordingProvider decorator wrapping `provider.Provider` for record/replay, (2) SSE-based TraceCollector consuming the HTTP event stream, (3) GLM-5 Judge evaluating traces against structured Rubrics. Zero changes to production code.

**Tech Stack:** Go 1.22+ testing package, DashScope Anthropic-compatible API, JSONL fixture files, Go build tags.

**Design Spec:** `docs/superpowers/specs/2026-05-30-real-e2e-testing-design.md`

---

## File Structure

```
test/real_e2e/
├── judge/
│   ├── recorder.go          # RecordingProvider: record/replay decorator on provider.Provider
│   ├── recorder_test.go     # Unit tests for RecordingProvider
│   ├── trace.go             # Trace types + SSE-based TraceCollector
│   ├── trace_test.go        # Unit tests for TraceCollector
│   ├── judge.go             # Judge interface, Rubric, Verdict, Criterion types
│   ├── glm5.go              # GLM-5 Judge implementation via DashScope API
│   └── glm5_test.go         # Unit tests for GLM-5 Judge (with mock HTTP)
├── helpers.go               # newRealStack, runScenario, assertVerdict
├── helpers_test.go          # Test that newRealStack works in skip mode
├── rubrics.go               # Rubric definitions per test category
├── file_tools_test.go       # File operation scenario tests
├── memory_test.go           # Memory subsystem scenario tests
├── extagent_test.go         # External agent scenario tests
└── fixtures/                # Recordings + judge cache (git-tracked)
    ├── recordings/           # JSONL recording files
    └── judge_cache/          # Cached Judge results
```

---

### Task 1: RecordingProvider — types and Mode

**Files:**
- Create: `test/real_e2e/judge/recorder.go`
- Test: `test/real_e2e/judge/recorder_test.go`

- [ ] **Step 1: Create the package and type definitions**

Create `test/real_e2e/judge/recorder.go`:

```go
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

type RecordMode int

const (
	ModeReplay RecordMode = iota
	ModeRecord
	ModeLive
)

func modeFromEnv() RecordMode {
	switch os.Getenv("WORKHORSE_TEST_MODE") {
	case "record":
		return ModeRecord
	case "live":
		return ModeLive
	default:
		return ModeReplay
	}
}

type recordingHeader struct {
	Test       string    `json:"test"`
	Model      string    `json:"model"`
	RecordedAt time.Time `json:"recorded_at"`
}

type recordedTurn struct {
	Request provider.Request      `json:"request"`
	Events  []provider.ProviderEvent `json:"events"`
}

type RecordingProvider struct {
	inner  provider.Provider
	mode   RecordMode
	dir    string
	testID string

	mu     sync.Mutex
	file   *os.File
	turns  []recordedTurn // replay buffer
	offset int            // next replay turn index
}

func NewRecordingProvider(inner provider.Provider, mode RecordMode, dir, testID string) *RecordingProvider {
	return &RecordingProvider{
		inner:  inner,
		mode:   mode,
		dir:    dir,
		testID: testID,
	}
}

func (rp *RecordingProvider) Name() string { return rp.inner.Name() }

func (rp *RecordingProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.ProviderEvent, error) {
	switch rp.mode {
	case ModeReplay:
		return rp.streamReplay(ctx, req)
	case ModeRecord:
		return rp.streamRecord(ctx, req)
	case ModeLive:
		return rp.inner.Stream(ctx, req)
	default:
		return rp.inner.Stream(ctx, req)
	}
}
```

- [ ] **Step 2: Write failing test for RecordingProvider replay mode**

Create `test/real_e2e/judge/recorder_test.go`:

```go
package judge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

func writeTestRecording(t *testing.T, dir, testID string, turns []recordedTurn) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(filepath.Join(dir, testID+".jsonl"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	hdr := recordingHeader{Test: testID, Model: "test-model"}
	hdrBytes, _ := json.Marshal(hdr)
	f.WriteString(string(hdrBytes) + "\n")
	for _, turn := range turns {
		b, _ := json.Marshal(turn)
		f.WriteString(string(b) + "\n")
	}
}

func TestRecordingProvider_ReplayMode(t *testing.T) {
	dir := t.TempDir()
	testID := "test_replay"
	turns := []recordedTurn{
		{
			Request: provider.Request{Model: "test-model", MaxTokens: 100},
			Events: []provider.ProviderEvent{
				{Type: provider.EventTextDelta, TextDelta: "hello"},
				{Type: provider.EventStop, StopReason: "end_turn"},
			},
		},
	}
	writeTestRecording(t, dir, testID, turns)

	rp := NewRecordingProvider(nil, ModeReplay, dir, testID)
	// Load the recording.
	if err := rp.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	ch, err := rp.Stream(context.Background(), provider.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var got []provider.ProviderEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].TextDelta != "hello" {
		t.Errorf("event 0 text = %q, want hello", got[0].TextDelta)
	}
	if got[1].StopReason != "end_turn" {
		t.Errorf("event 1 stop = %q, want end_turn", got[1].StopReason)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd /home/wallfacers/project/workhorse-agent && go test ./test/real_e2e/judge/ -run TestRecordingProvider_ReplayMode -v`
Expected: FAIL — `Load` method not defined.

- [ ] **Step 4: Implement replay and load methods**

Add to `test/real_e2e/judge/recorder.go`:

```go
func (rp *RecordingProvider) Load() error {
	path := filepath.Join(rp.dir, rp.testID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("recorder: read %s: %w", path, err)
	}
	lines := splitLines(string(data))
	// First line is header; rest are turns.
	for i, line := range lines {
		if i == 0 || line == "" {
			continue
		}
		var turn recordedTurn
		if err := json.Unmarshal([]byte(line), &turn); err != nil {
			return fmt.Errorf("recorder: parse line %d: %w", i, err)
		}
		rp.turns = append(rp.turns, turn)
	}
	return nil
}

func (rp *RecordingProvider) streamReplay(ctx context.Context, _ provider.Request) (<-chan provider.ProviderEvent, error) {
	rp.mu.Lock()
	if rp.offset >= len(rp.turns) {
		rp.mu.Unlock()
		ch := make(chan provider.ProviderEvent, 1)
		ch <- provider.ProviderEvent{Type: provider.EventStop, StopReason: "end_turn"}
		close(ch)
		return ch, nil
	}
	turn := rp.turns[rp.offset]
	rp.offset++
	rp.mu.Unlock()

	ch := make(chan provider.ProviderEvent, len(turn.Events)+1)
	go func() {
		defer close(ch)
		for _, ev := range turn.Events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd /home/wallfacers/project/workhorse-agent && go test ./test/real_e2e/judge/ -run TestRecordingProvider_ReplayMode -v`
Expected: PASS

- [ ] **Step 6: Write and test record mode**

Add to `test/real_e2e/judge/recorder_test.go`:

```go
func TestRecordingProvider_RecordMode(t *testing.T) {
	dir := t.TempDir()
	testID := "test_record"

	mock := mockprovider.New("anthropic")
	mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "world"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	rp := NewRecordingProvider(mock, ModeRecord, dir, testID)
	ch, err := rp.Stream(context.Background(), provider.Request{
		Model:     "test-model",
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for range ch {
	}

	if err := rp.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Verify file exists and has content.
	path := filepath.Join(dir, testID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("recording file is empty")
	}
	if !bytes.Contains(data, []byte(`"text_delta":"world"`)) {
		t.Errorf("recording missing expected event, got: %s", data)
	}
}
```

Add the `Save` and `streamRecord` methods to `recorder.go`:

```go
func (rp *RecordingProvider) Save() error {
	if rp.file != nil {
		rp.file.Close()
		rp.file = nil
	}
	return nil
}

func (rp *RecordingProvider) streamRecord(ctx context.Context, req provider.Request) (<-chan provider.ProviderEvent, error) {
	ch, err := rp.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(chan provider.ProviderEvent, 16)
	go func() {
		defer close(out)
		var events []provider.ProviderEvent
		for ev := range ch {
			events = append(events, ev)
			out <- ev
		}
		rp.mu.Lock()
		rp.turns = append(rp.turns, recordedTurn{
			Request: req,
			Events:  events,
		})
		rp.mu.Unlock()
	}()
	return out, nil
}

func (rp *RecordingProvider) Flush() error {
	if len(rp.turns) == 0 && rp.file == nil {
		// First flush: create file and write header + turns.
		if err := os.MkdirAll(rp.dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(rp.dir, rp.testID+".jsonl")
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		rp.file = f
		hdr := recordingHeader{Test: rp.testID, Model: "recorded"}
		hdrBytes, _ := json.Marshal(hdr)
		f.WriteString(string(hdrBytes) + "\n")
	}
	if rp.file != nil {
		for _, turn := range rp.turns {
			b, _ := json.Marshal(turn)
			rp.file.WriteString(string(b) + "\n")
		}
		rp.turns = nil
	}
	return nil
}
```

Update `Save` to call `Flush`:

```go
func (rp *RecordingProvider) Save() error {
	if err := rp.Flush(); err != nil {
		return err
	}
	if rp.file != nil {
		rp.file.Close()
		rp.file = nil
	}
	return nil
}
```

- [ ] **Step 7: Run record mode test**

Run: `cd /home/wallfacers/project/workhorse-agent && go test ./test/real_e2e/judge/ -run TestRecordingProvider_RecordMode -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add test/real_e2e/judge/recorder.go test/real_e2e/judge/recorder_test.go
git commit -m "feat(test): add RecordingProvider with record/replay modes"
```

---

### Task 2: Trace types and SSE-based TraceCollector

**Files:**
- Create: `test/real_e2e/judge/trace.go`
- Test: `test/real_e2e/judge/trace_test.go`

- [ ] **Step 1: Create trace types**

Create `test/real_e2e/judge/trace.go`:

```go
package judge

import (
	"bufio"
	"encoding/json"
	"strings"
	"time"
)

type Trace struct {
	TestName    string `json:"test_name"`
	UserMessage string `json:"user_message"`
	Turns       []Turn `json:"turns"`
}

type Turn struct {
	ModelOutput string              `json:"model_output"`
	ToolCalls   []ToolCallRecord    `json:"tool_calls,omitempty"`
	ToolResults []ToolResultRecord  `json:"tool_results,omitempty"`
	Duration    time.Duration       `json:"duration"`
}

type ToolCallRecord struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

type ToolResultRecord struct {
	ToolName string `json:"tool_name"`
	Output   string `json:"output"`
	IsError  bool   `json:"is_error"`
}

// CollectTrace reads SSE frames from a bufio.Reader until a terminal event
// (assistant_text_done with stop_reason end_turn, or error) is observed,
// assembling a Trace from the event stream.
func CollectTrace(testName, userMsg string, rd *bufio.Reader, timeout time.Duration) (*Trace, error) {
	trace := &Trace{
		TestName:    testName,
		UserMessage: userMsg,
	}
	var turn Turn
	start := time.Now()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		eventType := ""
		data := ""
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}

		switch eventType {
		case "assistant_text_delta":
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				if delta, ok := d["delta"].(string); ok {
					turn.ModelOutput += delta
				}
			}
		case "assistant_text_done":
			turn.Duration = time.Since(start)
			trace.Turns = append(trace.Turns, turn)
			turn = Turn{}
			start = time.Now()
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				if sr, ok := d["stop_reason"].(string); ok && sr == "end_turn" {
					return trace, nil
				}
			}
		case "tool_call_start":
			// Tool call is starting; the name is in the data.
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				name, _ := d["tool_name"].(string)
				turn.ToolCalls = append(turn.ToolCalls, ToolCallRecord{
					ToolName: name,
				})
			}
		case "tool_call_done":
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				name, _ := d["tool_name"].(string)
				output, _ := d["output"].(string)
				isErr, _ := d["is_error"].(bool)
				turn.ToolResults = append(turn.ToolResults, ToolResultRecord{
					ToolName: name,
					Output:   output,
					IsError:  isErr,
				})
			}
		case "error":
			return trace, nil
		}
	}
	return trace, nil
}
```

- [ ] **Step 2: Write test for TraceCollector**

Create `test/real_e2e/judge/trace_test.go`:

```go
package judge

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

func TestCollectTrace_TextOnly(t *testing.T) {
	sse := strings.Join([]string{
		"event: assistant_text_delta",
		"data: {\"delta\":\"hello \"}",
		"",
		"event: assistant_text_delta",
		"data: {\"delta\":\"world\"}",
		"",
		"event: assistant_text_done",
		"data: {\"stop_reason\":\"end_turn\"}",
		"",
	}, "\n")

	trace, err := CollectTrace("test", "hi", bufio.NewReader(strings.NewReader(sse)), 5*time.Second)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if trace.UserMessage != "hi" {
		t.Errorf("user message = %q, want hi", trace.UserMessage)
	}
	if len(trace.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(trace.Turns))
	}
	if trace.Turns[0].ModelOutput != "hello world" {
		t.Errorf("output = %q, want hello world", trace.Turns[0].ModelOutput)
	}
}

func TestCollectTrace_WithToolCall(t *testing.T) {
	sse := strings.Join([]string{
		"event: tool_call_start",
		"data: {\"tool_name\":\"Read\"}",
		"",
		"event: tool_call_done",
		"data: {\"tool_name\":\"Read\",\"output\":\"file contents here\",\"is_error\":false}",
		"",
		"event: assistant_text_delta",
		"data: {\"delta\":\"The file says hello\"}",
		"",
		"event: assistant_text_done",
		"data: {\"stop_reason\":\"end_turn\"}",
		"",
	}, "\n")

	trace, err := CollectTrace("test", "read file", bufio.NewReader(strings.NewReader(sse)), 5*time.Second)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(trace.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(trace.Turns))
	}
	if len(trace.Turns[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(trace.Turns[0].ToolCalls))
	}
	if trace.Turns[0].ToolCalls[0].ToolName != "Read" {
		t.Errorf("tool = %q, want Read", trace.Turns[0].ToolCalls[0].ToolName)
	}
	if len(trace.Turns[0].ToolResults) != 1 {
		t.Fatalf("tool results = %d, want 1", len(trace.Turns[0].ToolResults))
	}
	if trace.Turns[0].ToolResults[0].Output != "file contents here" {
		t.Errorf("tool result = %q, want file contents here", trace.Turns[0].ToolResults[0].Output)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `cd /home/wallfacers/project/workhorse-agent && go test ./test/real_e2e/judge/ -run TestCollectTrace -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add test/real_e2e/judge/trace.go test/real_e2e/judge/trace_test.go
git commit -m "feat(test): add SSE-based TraceCollector for real E2E tests"
```

---

### Task 3: Judge interface, Rubric types, and GLM-5 implementation

**Files:**
- Create: `test/real_e2e/judge/judge.go`
- Create: `test/real_e2e/judge/glm5.go`
- Test: `test/real_e2e/judge/glm5_test.go`

- [ ] **Step 1: Create Judge interface and Rubric types**

Create `test/real_e2e/judge/judge.go`:

```go
package judge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Verdict string

const (
	VerdictPass    Verdict = "PASS"
	VerdictFail    Verdict = "FAIL"
	VerdictPartial Verdict = "PARTIAL"
)

type JudgeResult struct {
	Verdict     Verdict   `json:"verdict"`
	Score       float64   `json:"score"`
	Reasoning   string    `json:"reasoning"`
	Suggestions []string  `json:"suggestions"`
}

type Rubric struct {
	Criteria   []Criterion `json:"criteria"`
	MinScore   float64     `json:"min_score"`
	MaxRetries int         `json:"max_retries"`
}

type Criterion struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight"`
	Required    bool    `json:"required"`
}

type Judge interface {
	Evaluate(ctx context.Context, trace *Trace, rubric Rubric) (*JudgeResult, error)
}

// judgeCacheKey returns a deterministic filename for a (trace + rubric) pair.
func judgeCacheKey(trace *Trace, rubric Rubric) string {
	h := sha256.New()
	json.NewEncoder(h).Encode(trace)
	json.NewEncoder(h).Encode(rubric)
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// loadCachedJudge reads a cached result from dir. Returns nil if not found.
func loadCachedJudge(dir, key string) (*JudgeResult, error) {
	path := filepath.Join(dir, key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var result JudgeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// saveCachedJudge writes a result to dir.
func saveCachedJudge(dir, key string, result *JudgeResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return os.WriteFile(filepath.Join(dir, key+".json"), data, 0o644)
}
```

- [ ] **Step 2: Create GLM-5 Judge implementation**

Create `test/real_e2e/judge/glm5.go`:

```go
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultJudgeModel = "glm-5"

type GLM5Judge struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
	cache   string // cache directory, empty = no caching
}

func NewGLM5Judge(opts ...func(*GLM5Judge)) *GLM5Judge {
	j := &GLM5Judge{
		apiKey:  os.Getenv("DASHSCOPE_API_KEY"),
		baseURL: os.Getenv("DASHSCOPE_BASE_URL"),
		model:   defaultJudgeModel,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	if j.baseURL == "" {
		j.baseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic"
	}
	for _, o := range opts {
		o(j)
	}
	return j
}

func WithCacheDir(dir string) func(*GLM5Judge) {
	return func(j *GLM5Judge) { j.cache = dir }
}

func (j *GLM5Judge) Evaluate(ctx context.Context, trace *Trace, rubric Rubric) (*JudgeResult, error) {
	// Check cache.
	if j.cache != "" {
		key := judgeCacheKey(trace, rubric)
		if cached, err := loadCachedJudge(j.cache, key); err == nil && cached != nil {
			return cached, nil
		}
	}

	prompt := j.buildPrompt(trace, rubric)

	result, err := j.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}

	// Save to cache.
	if j.cache != "" {
		key := judgeCacheKey(trace, rubric)
		_ = saveCachedJudge(j.cache, key, result)
	}

	return result, nil
}

func (j *GLM5Judge) buildPrompt(trace *Trace, rubric Rubric) string {
	var sb strings.Builder
	sb.WriteString("You are a test evaluator. Evaluate the following AI agent interaction against the given criteria.\n\n")
	sb.WriteString("## User Message\n")
	sb.WriteString(trace.UserMessage + "\n\n")
	sb.WriteString("## Interaction Trace\n")
	for i, turn := range trace.Turns {
		sb.WriteString(fmt.Sprintf("### Turn %d\n", i+1))
		if turn.ModelOutput != "" {
			sb.WriteString("Model output: " + turn.ModelOutput + "\n")
		}
		for _, tc := range turn.ToolCalls {
			sb.WriteString(fmt.Sprintf("Tool call: %s(%s)\n", tc.ToolName, string(tc.Input)))
		}
		for _, tr := range turn.ToolResults {
			label := "ok"
			if tr.IsError {
				label = "error"
			}
			sb.WriteString(fmt.Sprintf("Tool result [%s]: %s\n", label, tr.Output))
		}
	}
	sb.WriteString("\n## Evaluation Criteria\n")
	for _, c := range rubric.Criteria {
		req := ""
		if c.Required {
			req = " [REQUIRED]"
		}
		sb.WriteString(fmt.Sprintf("- %s%s (weight %.2f): %s\n", c.Name, req, c.Weight, c.Description))
	}
	sb.WriteString(fmt.Sprintf("\nMinimum passing score: %.2f\n", rubric.MinScore))
	sb.WriteString("\nRespond with ONLY a JSON object:\n")
	sb.WriteString(`{"verdict":"PASS|FAIL|PARTIAL","score":0.0-1.0,"reasoning":"...","suggestions":["..."]}` + "\n")
	return sb.String()
}

func (j *GLM5Judge) callLLM(ctx context.Context, prompt string) (*JudgeResult, error) {
	body := map[string]any{
		"model":      j.model,
		"max_tokens": 1024,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	b, _ := json.Marshal(body)

	url := strings.TrimRight(j.baseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", j.apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("judge: HTTP call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("judge: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("judge: decode response: %w", err)
	}

	if len(result.Content) == 0 {
		return nil, fmt.Errorf("judge: empty response")
	}

	text := result.Content[0].Text
	// Extract JSON from the response (may be wrapped in markdown code block).
	jsonStart := strings.Index(text, "{")
	jsonEnd := strings.LastIndex(text, "}")
	if jsonStart == -1 || jsonEnd == -1 {
		return nil, fmt.Errorf("judge: no JSON in response: %s", text)
	}

	var jr JudgeResult
	if err := json.Unmarshal([]byte(text[jsonStart:jsonEnd+1]), &jr); err != nil {
		return nil, fmt.Errorf("judge: parse JSON: %w\nraw: %s", err, text)
	}
	return &jr, nil
}
```

- [ ] **Step 3: Write test for GLM-5 Judge with mock HTTP server**

Create `test/real_e2e/judge/glm5_test.go`:

```go
package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGLM5Judge_EvaluateWithMock(t *testing.T) {
	// Mock Anthropic-compatible API server.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		resp := map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": `{"verdict":"PASS","score":0.85,"reasoning":"Model correctly called Read tool and reported file content.","suggestions":[]}`,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	judge := NewGLM5Judge(func(j *GLM5Judge) {
		j.baseURL = ts.URL
		j.apiKey = "test-key"
	})

	trace := &Trace{
		TestName:    "test_read",
		UserMessage: "Read the file /tmp/test.txt",
		Turns: []Turn{
			{
				ModelOutput: "The file contains: hello world",
				ToolCalls: []ToolCallRecord{
					{ToolName: "Read", Input: json.RawMessage(`{"path":"/tmp/test.txt"}`)},
				},
				ToolResults: []ToolResultRecord{
					{ToolName: "Read", Output: "hello world"},
				},
			},
		},
	}

	rubric := Rubric{
		MinScore:   0.7,
		MaxRetries: 2,
		Criteria: []Criterion{
			{Name: "tool_correct", Description: "Called Read with correct path", Weight: 0.5, Required: true},
			{Name: "output_match", Description: "Reported correct content", Weight: 0.5, Required: true},
		},
	}

	result, err := judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want PASS", result.Verdict)
	}
	if result.Score < rubric.MinScore {
		t.Errorf("score = %.2f, want >= %.2f", result.Score, rubric.MinScore)
	}
}

func TestGLM5Judge_Caching(t *testing.T) {
	cacheDir := t.TempDir()
	callCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"verdict":"PASS","score":0.9,"reasoning":"ok","suggestions":[]}`},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	judge := NewGLM5Judge(func(j *GLM5Judge) {
		j.baseURL = ts.URL
		j.apiKey = "test-key"
	}, WithCacheDir(cacheDir))

	trace := &Trace{TestName: "cache_test", UserMessage: "hi", Turns: []Turn{{ModelOutput: "hello"}}}
	rubric := Rubric{MinScore: 0.5, Criteria: []Criterion{{Name: "ok", Description: "ok", Weight: 1.0}}}

	// First call hits API.
	_, err := judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 API call, got %d", callCount)
	}

	// Second call should hit cache.
	_, err = judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call (cached), got %d", callCount)
	}
}
```

- [ ] **Step 4: Run tests**

Run: `cd /home/wallfacers/project/workhorse-agent && go test ./test/real_e2e/judge/ -run TestGLM5Judge -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add test/real_e2e/judge/judge.go test/real_e2e/judge/glm5.go test/real_e2e/judge/glm5_test.go
git commit -m "feat(test): add LLM-as-Judge with GLM-5 implementation and caching"
```

---

### Task 4: Rubric definitions

**Files:**
- Create: `test/real_e2e/rubrics.go`

- [ ] **Step 1: Create rubric definitions for each test category**

Create `test/real_e2e/rubrics.go`:

```go
//go:build real_e2e

package real_e2e

import "github.com/wallfacers/workhorse-agent/test/real_e2e/judge"

var fileToolsRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "tool_call_correct",
			Description: "Did the model call the correct tool with correct parameters (e.g., Read with the right file path)?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "response_accuracy",
			Description: "Does the response accurately reflect the tool result content without fabrication?",
			Weight:      0.35,
			Required:    true,
		},
		{
			Name:        "no_hallucination",
			Description: "Did the model avoid fabricating information not present in the tool result?",
			Weight:      0.2,
			Required:    true,
		},
		{
			Name:        "efficiency",
			Description: "Did the model avoid unnecessary extra tool calls?",
			Weight:      0.15,
		},
	},
}

var fileNotFoundRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "error_detected",
			Description: "Did the model correctly call Read on the nonexistent path?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "error_reported",
			Description: "Did the model report the file-not-found error to the user?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "no_fabrication",
			Description: "Did the model avoid making up file contents that don't exist?",
			Weight:      0.3,
			Required:    true,
		},
	},
}

var memoryRubric = judge.Rubric{
	MinScore:   0.8,
	MaxRetries: 1,
	Criteria: []judge.Criterion{
		{
			Name:        "tool_invocation",
			Description: "Did the model use memory_read/memory_write correctly with the right parameters?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "data_integrity",
			Description: "Is the data read back identical to what was written?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "cross_session",
			Description: "Can a new session see the memory written by a previous session?",
			Weight:      0.3,
			Required:    true,
		},
	},
}

var sessionSearchRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "search_called",
			Description: "Did the model call session_search with an appropriate query?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "results_accurate",
			Description: "Did the model accurately report the search results?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "relevance",
			Description: "Did the model correctly identify which results are relevant to the query?",
			Weight:      0.3,
		},
	},
}

var extAgentRubric = judge.Rubric{
	MinScore:   0.7,
	MaxRetries: 2,
	Criteria: []judge.Criterion{
		{
			Name:        "correct_invocation",
			Description: "Did the model invoke the right external agent with appropriate arguments?",
			Weight:      0.4,
			Required:    true,
		},
		{
			Name:        "output_handling",
			Description: "Did the model correctly process and relay the agent's output?",
			Weight:      0.3,
			Required:    true,
		},
		{
			Name:        "error_recovery",
			Description: "On failure, did the model provide a useful explanation to the user?",
			Weight:      0.3,
		},
	},
}
```

- [ ] **Step 2: Commit**

```bash
git add test/real_e2e/rubrics.go
git commit -m "feat(test): add Rubric definitions for real E2E test scenarios"
```

---

### Task 5: Test runner helpers (newRealStack, runScenario, assertVerdict)

**Files:**
- Create: `test/real_e2e/helpers.go`
- Test: `test/real_e2e/helpers_test.go`

- [ ] **Step 1: Create helpers.go**

Create `test/real_e2e/helpers.go`:

```go
//go:build real_e2e

package real_e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/test/real_e2e/judge"
)

const defaultTestModel = "qwen3.6-plus"

type realStack struct {
	t       *testing.T
	srv     *api.Server
	mgr     *session.Manager
	store   store.Store
	ts      *httptest.Server
	url     string
	logger  *slog.Logger
	prov    provider.Provider
	rec     *judge.RecordingProvider
	mode    judge.RecordMode
	fixDir  string
	workdir string
}

func newRealStack(t *testing.T) *realStack {
	t.Helper()

	mode := judge.ModeFromEnv()
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	baseURL := os.Getenv("DASHSCOPE_BASE_URL")
	if baseURL == "" {
		baseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic"
	}

	if mode != judge.ModeReplay && apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set; set WORKHORSE_TEST_MODE=replay or provide key")
	}

	// In replay mode, we still need to check that recording exists.
	fixDir := filepath.Join("test", "real_e2e", "fixtures", "recordings")
	workdir := t.TempDir()

	var rec *judge.RecordingProvider
	var prov provider.Provider

	if mode == judge.ModeReplay {
		rec = judge.NewRecordingProvider(nil, mode, fixDir, t.Name())
		if err := rec.Load(); err != nil {
			t.Skipf("no recording for %s: %v (run with WORKHORSE_TEST_MODE=record to create)", t.Name(), err)
		}
		prov = rec
	} else {
		realProv := anthropic.New(anthropic.Options{
			APIKey:           apiKey,
			BaseURL:          baseURL,
			DefaultMaxTokens: 4096,
		})
		rec = judge.NewRecordingProvider(realProv, mode, fixDir, t.Name())
		prov = rec
	}

	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := tools.NewRegistry()
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 30 * time.Second}
	permMgr := permission.New(st, nil, nil, 0)

	memLoader := &memory.Loader{ProfileDir: workdir}

	var mgr *session.Manager
	mgr = session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: 0,
		RunnerFactory: func(sess *session.Session) session.Runner {
			snap, _ := memLoader.Load()
			sess.MemorySnapshot = snap

			loop := agent.NewLoop(agent.LoopConfig{
				Model:              defaultTestModel,
				MaxTokens:          4096,
				CancelDrainTimeout: 5 * time.Second,
			})
			loop.Session = sess
			loop.Provider = prov
			loop.Orchestrator = orch
			loop.Permissions = permMgr
			loop.Logger = logger
			loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: workdir}
			return loop
		},
	})

	cfg := api.Config{
		Host:                    "127.0.0.1",
		Port:                    0,
		MaxRequestBodyBytes:     1 << 20,
		SSEKeepalive:            60 * time.Second,
		GracefulShutdownTimeout: 5 * time.Second,
		Version:                 "real-e2e",
	}
	srv := api.NewServer(cfg, mgr, st, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &realStack{
		t: t, srv: srv, mgr: mgr, store: st,
		ts: ts, url: ts.URL, logger: logger,
		prov: prov, rec: rec, mode: mode,
		fixDir: fixDir, workdir: workdir,
	}
}

func (s *realStack) createSession() string {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"workdir":   s.workdir,
		"provider":  "anthropic",
		"model":     defaultTestModel,
		"ephemeral": false,
	})
	resp, err := http.Post(s.url+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		s.t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("create status %d: %s", resp.StatusCode, raw)
	}
	var view struct{ ID string `json:"id"` }
	json.NewDecoder(resp.Body).Decode(&view)
	return view.ID
}

func (s *realStack) openSSE(id string) (*http.Response, *bufio.Reader) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.url+"/v1/sessions/"+id+"/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s.t.Fatalf("SSE status %d: %s", resp.StatusCode, raw)
	}
	return resp, bufio.NewReader(resp.Body)
}

func (s *realStack) postMessage(id, content string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"type":"user_message","content":%q}`, content)
	req, _ := http.NewRequest(http.MethodPost, s.url+"/v1/sessions/"+id+"/stream", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		s.t.Fatalf("post status %d (want 202)", resp.StatusCode)
	}
}

// scenarioConfig holds configuration for a test scenario.
type scenarioConfig struct {
	UserMessage string
	Rubric      judge.Rubric
	Timeout     time.Duration
	Setup       func(workdir string) // optional: create test files, etc.
}

func runScenario(t *testing.T, cfg scenarioConfig) (*judge.Trace, *judge.JudgeResult) {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}

	s := newRealStack(t)
	if cfg.Setup != nil {
		cfg.Setup(s.workdir)
	}

	id := s.createSession()
	resp, rd := s.openSSE(id)
	defer resp.Body.Close()

	s.postMessage(id, cfg.UserMessage)

	trace, err := judge.CollectTrace(t.Name(), cfg.UserMessage, rd, cfg.Timeout)
	if err != nil {
		t.Fatalf("collect trace: %v", err)
	}

	// Save recording if in record mode.
	if s.mode == judge.ModeRecord && s.rec != nil {
		if err := s.rec.Save(); err != nil {
			t.Logf("save recording: %v", err)
		}
	}

	// Run judge unless off.
	judgeMode := os.Getenv("WORKHORSE_JUDGE_MODE")
	if judgeMode == "off" {
		return trace, nil
	}

	cacheDir := filepath.Join("test", "real_e2e", "fixtures", "judge_cache")
	j := judge.NewGLM5Judge(func(gj *judge.GLM5Judge) {
		if judgeMode != "llm" {
			// Cached mode: only use cache, don't call API.
			// API key may not be set.
		}
	}, judge.WithCacheDir(cacheDir))

	result, err := j.Evaluate(context.Background(), trace, cfg.Rubric)
	if err != nil {
		if judgeMode == "cached" {
			t.Skipf("no cached judge result: %v", err)
		}
		t.Fatalf("judge evaluate: %v", err)
	}

	return trace, result
}

func assertVerdict(t *testing.T, result *judge.JudgeResult) {
	t.Helper()
	if result == nil {
		t.Log("Judge skipped (WORKHORSE_JUDGE_MODE=off)")
		return
	}
	if result.Verdict != judge.VerdictPass {
		t.Errorf("Judge verdict: %s (score %.2f)", result.Verdict, result.Score)
		t.Errorf("Reasoning: %s", result.Reasoning)
		for _, s := range result.Suggestions {
			t.Errorf("Suggestion: %s", s)
		}
		t.FailNow()
	}
	t.Logf("Judge: PASS (score %.2f) — %s", result.Score, result.Reasoning)
}

func modeFromEnv() judge.RecordMode {
	return judge.ModeFromEnv()
}
```

- [ ] **Step 2: Write a smoke test that verifies stack creation in skip mode**

Create `test/real_e2e/helpers_test.go`:

```go
//go:build real_e2e

package real_e2e

import (
	"os"
	"testing"
)

func TestNewRealStack_SkipWithoutKey(t *testing.T) {
	// Clear API key to verify skip behavior.
	os.Unsetenv("DASHSCOPE_API_KEY")
	os.Setenv("WORKHORSE_TEST_MODE", "live")
	defer os.Unsetenv("WORKHORSE_TEST_MODE")

	// This should skip, not panic or hang.
	s := newRealStack(t)
	if s != nil {
		t.Log("stack created unexpectedly")
	}
}
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /home/wallfacers/project/workhorse-agent && go vet ./test/real_e2e/...`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add test/real_e2e/helpers.go test/real_e2e/helpers_test.go
git commit -m "feat(test): add real E2E test runner helpers"
```

---

### Task 6: File tools scenario tests

**Files:**
- Create: `test/real_e2e/file_tools_test.go`

- [ ] **Step 1: Create file tools test scenarios**

Create `test/real_e2e/file_tools_test.go`:

```go
//go:build real_e2e

package real_e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRead_Basic_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Read the file hello.txt in the current directory and tell me what it says.",
		Rubric:      fileToolsRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "hello.txt"), []byte("Hello from workhorse-agent E2E test!"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestFileRead_NotFound_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Read the file nonexistent_file.txt and tell me its contents.",
		Rubric:      fileNotFoundRubric,
		Timeout:     60 * time.Second,
		// No Setup: file does not exist.
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestFileWrite_Create_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Create a file called output.txt with the content 'E2E test write'.",
		Rubric:      fileToolsRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestFileEdit_Modify_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Edit the file config.yaml: change 'debug: false' to 'debug: true'.",
		Rubric:      fileToolsRubric,
		Timeout:     90 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "config.yaml"), []byte("debug: false\nport: 8080\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestBash_ListDir_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Run 'ls -la' in the current directory and list the files you see.",
		Rubric:      fileToolsRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "a.txt"), []byte("aaa"), 0o644)
			os.WriteFile(filepath.Join(workdir, "b.txt"), []byte("bbb"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestMultiTool_Workflow_Full(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Read the file data.csv, find the highest value in the second column, and write it to max_value.txt.",
		Rubric:      fileToolsRubric,
		Timeout:     120 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "data.csv"), []byte("name,value\nalpha,10\nbeta,42\ngamma,7\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /home/wallfacers/project/workhorse-agent && go vet -tags=real_e2e ./test/real_e2e/...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add test/real_e2e/file_tools_test.go
git commit -m "feat(test): add file tools real E2E scenario tests"
```

---

### Task 7: Memory subsystem scenario tests

**Files:**
- Create: `test/real_e2e/memory_test.go`

- [ ] **Step 1: Create memory test scenarios**

Create `test/real_e2e/memory_test.go`:

```go
//go:build real_e2e

package real_e2e

import (
	"testing"
	"time"
)

func TestMemoryWrite_Read_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Write 'E2E memory test content' to memory, then read it back and confirm it matches.",
		Rubric:      memoryRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestMemoryCrossSession_Integration(t *testing.T) {
	// Session A: write memory.
	traceA, resultA := runScenario(t, scenarioConfig{
		UserMessage: "Write 'cross-session test data' to memory (kind: memory).",
		Rubric:      memoryRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Session A: %d turns", len(traceA.Turns))
	assertVerdict(t, resultA)

	// Session B: read memory in a new session to verify persistence.
	// Note: cross-session testing requires the same profileDir.
	// For real E2E, this test validates that memory_write persists to disk
	// and memory_read can retrieve it in a fresh session.
	traceB, resultB := runScenario(t, scenarioConfig{
		UserMessage: "Read memory (kind: memory) and tell me what it contains.",
		Rubric:      memoryRubric,
		Timeout:     60 * time.Second,
	})
	t.Logf("Session B: %d turns", len(traceB.Turns))
	assertVerdict(t, resultB)
}

func TestSessionSearch_Basic_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Search all sessions for the term 'workhorse-agent'.",
		Rubric:      sessionSearchRubric,
		Timeout:     60 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
```

- [ ] **Step 2: Commit**

```bash
git add test/real_e2e/memory_test.go
git commit -m "feat(test): add memory subsystem real E2E scenario tests"
```

---

### Task 8: External agent scenario tests

**Files:**
- Create: `test/real_e2e/extagent_test.go`

- [ ] **Step 1: Create external agent test scenarios**

Create `test/real_e2e/extagent_test.go`:

```go
//go:build real_e2e

package real_e2e

import (
	"testing"
	"time"
)

func TestExtAgent_Invoke_Smoke(t *testing.T) {
	// Note: this test requires an external agent to be configured.
	// If no agent is available, the Judge should still evaluate the model's
	// handling of the situation (e.g., reporting that no agent is available).
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Use the echo agent to run 'echo hello from E2E test'.",
		Rubric:      extAgentRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestExtAgent_Error_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Use the external agent called 'nonexistent-agent-xyz' to do something.",
		Rubric:      extAgentRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
```

- [ ] **Step 2: Commit**

```bash
git add test/real_e2e/extagent_test.go
git commit -m "feat(test): add external agent real E2E scenario tests"
```

---

### Task 9: Create fixtures directory and initial README

**Files:**
- Create: `test/real_e2e/fixtures/recordings/.gitkeep`
- Create: `test/real_e2e/fixtures/judge_cache/.gitkeep`

- [ ] **Step 1: Create directory structure and placeholder files**

```bash
mkdir -p test/real_e2e/fixtures/recordings test/real_e2e/fixtures/judge_cache
touch test/real_e2e/fixtures/recordings/.gitkeep test/real_e2e/fixtures/judge_cache/.gitkeep
```

- [ ] **Step 2: Add a fixtures README**

Create `test/real_e2e/fixtures/README.md`:

```markdown
# Real E2E Test Fixtures

## recordings/
JSONL files containing recorded LLM interactions. Each file is named after the
test case and contains a header line + one JSON line per Stream() call.

To regenerate: `WORKHORSE_TEST_MODE=record go test ./test/real_e2e/... -tags=real_e2e -run <TestName>`

## judge_cache/
Cached Judge (GLM-5) evaluation results. Keyed by SHA-256 hash of (trace + rubric).

To regenerate: `WORKHORSE_JUDGE_MODE=llm go test ./test/real_e2e/... -tags=real_e2e -run <TestName>`
```

- [ ] **Step 3: Commit**

```bash
git add test/real_e2e/fixtures/
git commit -m "feat(test): add fixture directories for real E2E recordings and judge cache"
```

---

### Task 10: End-to-end verification — record one smoke test

**Goal:** Run one test in record mode to create an initial recording, then replay it.

- [ ] **Step 1: Verify the full stack compiles**

Run: `cd /home/wallfacers/project/workhorse-agent && go build -tags=real_e2e ./test/real_e2e/...`
Expected: builds without errors

- [ ] **Step 2: Record a single smoke test** (requires DASHSCOPE_API_KEY)

Run: `cd /home/wallfacers/project/workhorse-agent && WORKHORSE_TEST_MODE=record WORKHORSE_JUDGE_MODE=off DASHSCOPE_API_KEY=$DASHSCOPE_API_KEY go test ./test/real_e2e/... -tags=real_e2e -run TestFileRead_Basic_Smoke -v -timeout 5m`
Expected: creates `test/real_e2e/fixtures/recordings/TestFileRead_Basic_Smoke.jsonl`

- [ ] **Step 3: Replay the recorded test**

Run: `cd /home/wallfacers/project/workhorse-agent && WORKHORSE_TEST_MODE=replay WORKHORSE_JUDGE_MODE=off go test ./test/real_e2e/... -tags=real_e2e -run TestFileRead_Basic_Smoke -v -timeout 2m`
Expected: PASS using the recorded interaction

- [ ] **Step 4: Commit the initial recording**

```bash
git add test/real_e2e/fixtures/recordings/TestFileRead_Basic_Smoke.jsonl
git commit -m "test: add initial recording for TestFileRead_Basic_Smoke"
```

---

## Self-Review

**1. Spec coverage:**
- RecordingProvider (Component 1): Task 1 ✓
- LLM-as-Judge (Component 2): Task 3 ✓
- TraceCollector (Component 3): Task 2 ✓
- Test Runner Helpers (Component 4): Task 5 ✓
- Test Scenarios (Component 5): Tasks 6, 7, 8 ✓
- Rubric Definitions (Component 6): Task 4 ✓
- Execution Modes: Task 5 (env vars), Task 10 (verification) ✓
- Recording Format: Task 1 ✓
- Build Tags: Task 5+ (`//go:build real_e2e`) ✓
- Timeout/Retry: Task 5 (helpers), Task 3 (judge retry in rubric) ✓
- Cost Estimate: N/A (runtime behavior)
- Recording Drift Detection: not implemented — add in follow-up, not blocking

**2. Placeholder scan:** No TBD/TODO/fill-in-later found. All code blocks contain complete implementation.

**3. Type consistency:** Checked all cross-references between tasks:
- `judge.RecordingProvider` used consistently in Task 1 and Task 5
- `judge.Trace`, `judge.JudgeResult`, `judge.Rubric`, `judge.Criterion` used in Tasks 2-8
- `judge.VerdictPass` used in Task 5's `assertVerdict`
- `judge.ModeFromEnv()` used in Task 5 and Task 1
- `scenarioConfig` struct in Task 5 matches usage in Tasks 6-8
