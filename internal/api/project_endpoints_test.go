package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// seedSession persists a session row directly (not live in the manager).
func seedSession(t *testing.T, st store.Store, id, workdir, title string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.CreateSession(context.Background(), &store.Session{
		ID: id, State: store.SessionStateIdle, Workdir: workdir, EnvJSON: "{}",
		Title: title, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func newProjectServer(t *testing.T) (store.Store, *session.Manager, string) {
	t.Helper()
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host: "127.0.0.1", Port: 0, MaxRequestBodyBytes: 1 << 20, Version: "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)
	return st, mgr, ts.URL
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return body
}

func TestListByWorkdir_CamelCaseAndStatus(t *testing.T) {
	st, _, url := newProjectServer(t)
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FA1", "/proj/a", "first chat")
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FA2", "/proj/a", "second chat")
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FA3", "/proj/b", "other project")

	body := getJSON(t, url+"/v1/sessions?workdir=/proj/a")
	sessions, ok := body["sessions"].([]any)
	if !ok || len(sessions) != 2 {
		t.Fatalf("want 2 sessions for /proj/a, got %v", body["sessions"])
	}
	m := sessions[0].(map[string]any)
	// camelCase keys must be present; snake_case must NOT.
	for _, k := range []string{"id", "workdir", "title", "status"} {
		if _, ok := m[k]; !ok {
			t.Errorf("SessionMeta missing required key %q: %v", k, m)
		}
	}
	if _, bad := m["created_at"]; bad {
		t.Errorf("response leaked snake_case created_at: %v", m)
	}
	if m["status"] != "idle" {
		t.Errorf("persisted session status = %v, want idle", m["status"])
	}
	if m["title"] == nil {
		t.Errorf("title must always be present")
	}
}

func TestListByWorkdir_SurvivesNoLiveSession(t *testing.T) {
	st, mgr, url := newProjectServer(t)
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FB1", "/proj/x", "persisted")

	// Not live in the manager — proves the listing is store-backed.
	if _, err := mgr.GetSession("01ARZ3NDEKTSV4RRFFQ69G5FB1"); err == nil {
		t.Fatal("precondition: session must not be live")
	}
	body := getJSON(t, url+"/v1/sessions?workdir=/proj/x")
	if sessions, _ := body["sessions"].([]any); len(sessions) != 1 {
		t.Fatalf("store-backed listing should return 1, got %v", body["sessions"])
	}
}

func TestHistory_ToolCallJoinAndReasoning(t *testing.T) {
	st, _, url := newProjectServer(t)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FC1"
	seedSession(t, st, id, "/proj/h", "history chat")
	ctx := context.Background()
	now := time.Now().UTC()

	// user text
	mustAppend(t, st, id, "u1", "user", `[{"type":"text","text":"run ls"}]`, now)
	// assistant: thinking + tool_use
	mustAppend(t, st, id, "a1", "assistant",
		`[{"type":"thinking","thinking":"I should list","signature":"sig"},`+
			`{"type":"tool_use","toolUseId":"call_1","toolName":"Bash","input":{"cmd":"ls"}}]`,
		now.Add(time.Millisecond))
	// user: tool_result (must merge into call_1, not become its own message)
	mustAppend(t, st, id, "u2", "user",
		`[{"type":"tool_result","toolUseId":"call_1","output":"a\nb","isError":false}]`,
		now.Add(2*time.Millisecond))
	_ = ctx

	body := getJSON(t, url+"/v1/sessions/"+id+"/history")
	msgs, _ := body["messages"].([]any)
	// u1 (text) + a1 (reasoning+tool_call); the tool_result-only message is dropped.
	if len(msgs) != 2 {
		t.Fatalf("want 2 history messages, got %d: %v", len(msgs), msgs)
	}

	assistant := msgs[1].(map[string]any)
	parts := assistant["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("assistant parts = %d, want 2 (reasoning, tool_call)", len(parts))
	}
	reasoning := parts[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["status"] != "done" {
		t.Errorf("reasoning part must carry status=done: %v", reasoning)
	}
	tc := parts[1].(map[string]any)
	if tc["type"] != "tool_call" || tc["id"] != "call_1" || tc["name"] != "Bash" {
		t.Errorf("tool_call must use wire id/name: %v", tc)
	}
	if tc["status"] != "done" {
		t.Errorf("tool_call status after result = %v, want done", tc["status"])
	}
	if tc["output"] == nil {
		t.Errorf("tool_call output must be backfilled from tool_result: %v", tc)
	}
	// Storage field names must NOT leak to the wire.
	if _, bad := tc["toolUseId"]; bad {
		t.Errorf("wire tool_call leaked storage key toolUseId: %v", tc)
	}
}

func TestRenameSession_Persisted(t *testing.T) {
	st, _, url := newProjectServer(t)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FD1"
	seedSession(t, st, id, "/proj/r", "old name")

	req, _ := http.NewRequest(http.MethodPatch, url+"/v1/sessions/"+id,
		strings.NewReader(`{"title":"new name"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", resp.StatusCode)
	}
	var meta map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	if meta["title"] != "new name" {
		t.Errorf("PATCH response title = %v, want 'new name'", meta["title"])
	}
	// Persisted.
	row, _ := st.GetSession(context.Background(), id)
	if row.Title != "new name" {
		t.Errorf("store title = %q, want 'new name'", row.Title)
	}
}

func TestListProjects(t *testing.T) {
	st, _, url := newProjectServer(t)
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FE1", "/proj/p1", "")
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FE2", "/proj/p1", "")
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FE3", "/proj/p2", "")

	body := getJSON(t, url+"/v1/projects")
	projects, _ := body["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("want 2 projects, got %d: %v", len(projects), projects)
	}
	for _, p := range projects {
		pm := p.(map[string]any)
		if pm["path"] == nil {
			t.Errorf("project missing path: %v", pm)
		}
	}
}

func TestDeleteSession_HardPurgesTranscript(t *testing.T) {
	st, _, url := newProjectServer(t)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FF1"
	seedSession(t, st, id, "/proj/d", "to delete")
	mustAppend(t, st, id, "m1", "user", `[{"type":"text","text":"hi"}]`, time.Now().UTC())

	req, _ := http.NewRequest(http.MethodDelete, url+"/v1/sessions/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("DELETE status = %d, want 2xx", resp.StatusCode)
	}

	// Row and transcript are gone.
	if _, err := st.GetSession(context.Background(), id); err == nil {
		t.Error("session row should be hard-deleted")
	}
	if msgs, _ := st.ListMessages(context.Background(), id); len(msgs) != 0 {
		t.Errorf("transcript should be purged, got %d messages", len(msgs))
	}
	// And it no longer appears in the project listing.
	body := getJSON(t, url+"/v1/sessions?workdir=/proj/d")
	if sessions, _ := body["sessions"].([]any); len(sessions) != 0 {
		t.Errorf("deleted session still listed: %v", sessions)
	}
}

// TestListByWorkdir_MultiActiveStatus proves the listing distinguishes a
// running session from an idle one in the same project (T3): status is overlaid
// per-session from the live manager, independent of subscribers.
func TestListByWorkdir_MultiActiveStatus(t *testing.T) {
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host: "127.0.0.1", Port: 0, MaxRequestBodyBytes: 1 << 20, Version: "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)

	// Two live sessions in the same project.
	a, err := mgr.CreateSession(context.Background(), session.Options{Workdir: "/proj/m", Model: "m"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := mgr.CreateSession(context.Background(), session.Options{Workdir: "/proj/m", Model: "m"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	// Drive A into a running turn; B stays idle.
	if err := a.Transition(session.StateIdle, session.StateThinking); err != nil {
		t.Fatalf("transition: %v", err)
	}

	body := getJSON(t, ts.URL+"/v1/sessions?workdir=/proj/m")
	statuses := map[string]string{}
	for _, raw := range body["sessions"].([]any) {
		m := raw.(map[string]any)
		statuses[m["id"].(string)] = m["status"].(string)
	}
	if statuses[a.ID] != "running" {
		t.Errorf("session A status = %q, want running", statuses[a.ID])
	}
	if statuses[b.ID] != "idle" {
		t.Errorf("session B status = %q, want idle", statuses[b.ID])
	}
}

func mustAppend(t *testing.T, st store.Store, sessionID, msgID, role, contentJSON string, at time.Time) {
	t.Helper()
	if err := st.AppendMessage(context.Background(), &store.Message{
		ID: msgID, SessionID: sessionID, Role: role, ContentJSON: contentJSON, CreatedAt: at,
	}); err != nil {
		t.Fatalf("append %s: %v", msgID, err)
	}
}

// deleteProject issues DELETE /v1/projects?workdir= and returns status + body.
func deleteProject(t *testing.T, baseURL, workdir string) (int, map[string]any) {
	t.Helper()
	u := baseURL + "/v1/projects"
	if workdir != "" {
		u += "?workdir=" + url.QueryEscape(workdir)
	}
	req, _ := http.NewRequest(http.MethodDelete, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", u, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// A project is a derived view (distinct workdir with >=1 non-deleted session), so
// deleting it hard-purges every session under that workdir and leaves siblings.
func TestDeleteProject_PurgesAllSessionsForWorkdir(t *testing.T) {
	st, _, url := newProjectServer(t)
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FB1", "/proj/del", "one")
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FB2", "/proj/del", "two")
	mustAppend(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FB1", "m1", "user", `[{"type":"text","text":"hi"}]`, time.Now().UTC())
	seedSession(t, st, "01ARZ3NDEKTSV4RRFFQ69G5FB3", "/proj/keep", "other")

	status, body := deleteProject(t, url, "/proj/del")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := body["deleted"]; got != float64(2) {
		t.Fatalf("deleted = %v, want 2", got)
	}

	// Rows + transcripts gone; the workdir vanishes from /v1/projects; sibling stays.
	if _, err := st.GetSession(context.Background(), "01ARZ3NDEKTSV4RRFFQ69G5FB1"); err == nil {
		t.Error("session row should be hard-deleted")
	}
	if msgs, _ := st.ListMessages(context.Background(), "01ARZ3NDEKTSV4RRFFQ69G5FB1"); len(msgs) != 0 {
		t.Errorf("transcript should be purged, got %d messages", len(msgs))
	}
	for _, p := range getJSON(t, url+"/v1/projects")["projects"].([]any) {
		if p.(map[string]any)["path"] == "/proj/del" {
			t.Fatalf("/proj/del still listed after delete")
		}
	}
	if sessions, _ := getJSON(t, url+"/v1/sessions?workdir=/proj/del")["sessions"].([]any); len(sessions) != 0 {
		t.Errorf("want 0 sessions for /proj/del, got %v", sessions)
	}
	if sessions, _ := getJSON(t, url+"/v1/sessions?workdir=/proj/keep")["sessions"].([]any); len(sessions) != 1 {
		t.Errorf("sibling project /proj/keep should be untouched, got %v", sessions)
	}
}

func TestDeleteProject_MissingWorkdirIs400(t *testing.T) {
	_, _, url := newProjectServer(t)
	if status, _ := deleteProject(t, url, ""); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestDeleteProject_EmptyWorkdirIsIdempotent(t *testing.T) {
	_, _, url := newProjectServer(t)
	status, body := deleteProject(t, url, "/proj/never-existed")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := body["deleted"]; got != float64(0) {
		t.Fatalf("deleted = %v, want 0", got)
	}
}
