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
	"runtime"
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

var projectRoot string

func init() {
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot = filepath.Join(filepath.Dir(thisFile), "..", "..")
}

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

	fixDir := filepath.Join(projectRoot, "test", "real_e2e", "fixtures", "recordings")
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

type scenarioConfig struct {
	UserMessage string
	Rubric      judge.Rubric
	Timeout     time.Duration
	Setup       func(workdir string)
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

	if s.mode == judge.ModeRecord && s.rec != nil {
		if saveErr := s.rec.Save(); saveErr != nil {
			t.Logf("save recording: %v", saveErr)
		}
	}

	judgeMode := os.Getenv("WORKHORSE_JUDGE_MODE")
	if judgeMode == "off" {
		return trace, nil
	}

	cacheDir := filepath.Join(projectRoot, "test", "real_e2e", "fixtures", "judge_cache")
	j := judge.NewGLM5Judge(func(gj *judge.GLM5Judge) {}, judge.WithCacheDir(cacheDir))

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
