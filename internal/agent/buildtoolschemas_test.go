package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// tsStub is a configurable tool: defer=true makes it Deferrable.
type tsStub struct {
	name   string
	desc   string
	schema string
}

func (s tsStub) Name() string        { return s.name }
func (s tsStub) Description() string { return s.desc }
func (s tsStub) InputSchema() json.RawMessage {
	if s.schema == "" {
		return []byte(`{}`)
	}
	return []byte(s.schema)
}
func (s tsStub) IsReadOnly() bool              { return true }
func (s tsStub) CanRunInParallel() bool        { return true }
func (s tsStub) DefaultTimeout() time.Duration { return 0 }
func (s tsStub) Run(context.Context, *tools.Env, json.RawMessage) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

// deferStub adds ShouldDefer so it satisfies tools.Deferrable.
type deferStub struct{ tsStub }

func (d deferStub) ShouldDefer() bool { return true }

func newBuildLoop(mode string, percent int, regTools ...tools.Tool) *Loop {
	reg := tools.NewRegistry()
	for _, t := range regTools {
		_ = reg.Register(t)
	}
	l := NewLoop(LoopConfig{ToolSearchMode: mode, ToolSearchPercent: percent, MaxHistoryTokens: 200_000})
	l.Registry = reg
	l.Session = session.New(session.Options{Ephemeral: true})
	l.ToolEnv = &tools.Env{}
	return l
}

func schemaNames(l *Loop) ([]string, []string) {
	schemas, deferred := l.buildToolSchemas()
	got := make([]string, len(schemas))
	for i, s := range schemas {
		got[i] = s.Name
	}
	return got, deferred
}

func has(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func toolSearchStub() tsStub { return tsStub{name: tools.ToolSearchName, desc: "search"} }

// ---- 5.1 tst mode mixed list ----

func TestBuild_TST_DefersUndiscovered(t *testing.T) {
	l := newBuildLoop("tst", 0,
		tsStub{name: "Read", desc: "read a file"},
		deferStub{tsStub{name: "slack__send", desc: "send slack"}},
		toolSearchStub(),
	)
	schemas, deferred := schemaNames(l)
	if !has(schemas, "Read") {
		t.Errorf("Read (non-deferrable) must be present, got %v", schemas)
	}
	if !has(schemas, tools.ToolSearchName) {
		t.Errorf("ToolSearch must be present when deferral active, got %v", schemas)
	}
	if has(schemas, "slack__send") {
		t.Errorf("undiscovered deferrable tool must NOT have full schema, got %v", schemas)
	}
	if !has(deferred, "slack__send") {
		t.Errorf("slack__send should be announced as deferred, got %v", deferred)
	}
	// Catalog exposed to ToolSearch contains exactly the deferred tool.
	cat, ok := l.ToolEnv.ToolCatalog.(tools.ToolCatalog)
	if !ok {
		t.Fatalf("ToolCatalog not set")
	}
	infos := cat.DeferredTools()
	if len(infos) != 1 || infos[0].Name != "slack__send" {
		t.Errorf("catalog should hold slack__send only, got %v", infos)
	}
}

// ---- 5.2 discovered tool is included ----

func TestBuild_DiscoveredToolIncluded(t *testing.T) {
	l := newBuildLoop("tst", 0,
		deferStub{tsStub{name: "slack__send", desc: "send slack"}},
		toolSearchStub(),
	)
	l.Session.MarkToolsDiscovered([]string{"slack__send"})
	schemas, deferred := schemaNames(l)
	if !has(schemas, "slack__send") {
		t.Errorf("discovered tool must have full schema, got %v", schemas)
	}
	if has(deferred, "slack__send") {
		t.Errorf("discovered tool must not be announced as deferred, got %v", deferred)
	}
}

// ---- 5.3 standard mode parity ----

func TestBuild_Standard_NoDeferralNoToolSearch(t *testing.T) {
	l := newBuildLoop("standard", 0,
		tsStub{name: "Read", desc: "read"},
		deferStub{tsStub{name: "slack__send", desc: "send"}},
		toolSearchStub(),
	)
	schemas, deferred := schemaNames(l)
	if !has(schemas, "Read") || !has(schemas, "slack__send") {
		t.Errorf("standard mode must expose all tools with schema, got %v", schemas)
	}
	if has(schemas, tools.ToolSearchName) {
		t.Errorf("ToolSearch must be absent in standard mode, got %v", schemas)
	}
	if len(deferred) != 0 {
		t.Errorf("standard mode must announce nothing, got %v", deferred)
	}
	if l.ToolEnv.ToolCatalog != nil {
		t.Errorf("standard mode must not set ToolCatalog")
	}
}

// ---- 5.4 AllowedTools precedence ----

func TestBuild_AllowedToolsExcludesDeferred(t *testing.T) {
	l := newBuildLoop("tst", 0,
		tsStub{name: "Read", desc: "read"},
		deferStub{tsStub{name: "slack__send", desc: "send"}},
		toolSearchStub(),
	)
	l.Session.SetAllowedTools([]string{"Read", tools.ToolSearchName}) // exclude slack__send
	schemas, deferred := schemaNames(l)
	if has(schemas, "slack__send") || has(deferred, "slack__send") {
		t.Errorf("AllowedTools-excluded tool must appear nowhere; schemas=%v deferred=%v", schemas, deferred)
	}
}

// ---- 4.3 auto threshold ----

func TestBuild_Auto_BelowThresholdNoDefer(t *testing.T) {
	// One tiny deferrable tool, threshold 10% of 200k tokens — far above.
	l := newBuildLoop("auto", 10,
		deferStub{tsStub{name: "slack__send", desc: "send"}},
		toolSearchStub(),
	)
	schemas, deferred := schemaNames(l)
	if !has(schemas, "slack__send") {
		t.Errorf("below-threshold deferrable tool should load fully, got %v", schemas)
	}
	if has(schemas, tools.ToolSearchName) || len(deferred) != 0 {
		t.Errorf("below threshold: no deferral expected; schemas=%v deferred=%v", schemas, deferred)
	}
}

func TestBuild_Auto_AboveThresholdDefers(t *testing.T) {
	// Build a huge description so chars/4 crosses 10% of a tiny MaxHistoryTokens.
	big := strings.Repeat("x", 5000)
	l := newBuildLoop("auto", 10, deferStub{tsStub{name: "slack__send", desc: big}}, toolSearchStub())
	l.Config.MaxHistoryTokens = 1000 // threshold = 100 tokens; 5000 chars ≈ 1250 tokens
	schemas, deferred := schemaNames(l)
	if has(schemas, "slack__send") {
		t.Errorf("above-threshold deferrable tool should be deferred, got %v", schemas)
	}
	if !has(deferred, "slack__send") {
		t.Errorf("above threshold: slack__send should be announced, got %v", deferred)
	}
}
