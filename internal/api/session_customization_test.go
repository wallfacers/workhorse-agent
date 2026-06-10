package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func postSession(t *testing.T, url string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url+"/v1/sessions", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// Spec scenario: 创建带定制字段的会话.
func TestCreateSession_InstructionsAndMetadata(t *testing.T) {
	s, ts := newTestServer(t)
	resp, data := postSession(t, ts.URL, map[string]any{
		"workdir": "/tmp/proj", "provider": "anthropic", "model": "m",
		"ephemeral":    true,
		"instructions": "当前页面 taskId=T-1024",
		"metadata":     map[string]string{"dataweave_conversation_id": "conv-7f3a"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var view sessionMeta
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if view.Metadata["dataweave_conversation_id"] != "conv-7f3a" {
		t.Fatalf("create response metadata lost: %+v", view)
	}

	// GET reflects the same metadata, and the live session carries instructions.
	getResp, err := http.Get(ts.URL + "/v1/sessions/" + view.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var got sessionMeta
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata["dataweave_conversation_id"] != "conv-7f3a" {
		t.Fatalf("GET metadata lost: %+v", got)
	}
	live, err := s.manager.GetSession(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Instructions != "当前页面 taskId=T-1024" {
		t.Fatalf("live instructions lost: %q", live.Instructions)
	}
}

// Spec scenario: 超限拒绝.
func TestCreateSession_CustomizationLimits(t *testing.T) {
	_, ts := newTestServer(t)

	resp, data := postSession(t, ts.URL, map[string]any{
		"workdir": "/tmp/proj", "provider": "p", "model": "m", "ephemeral": true,
		"instructions": strings.Repeat("a", maxInstructionsBytes+1),
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "instructions") {
		t.Fatalf("oversized instructions: status %d body %s", resp.StatusCode, data)
	}

	meta := map[string]string{}
	for i := 0; i < maxMetadataKeys+1; i++ {
		meta[strings.Repeat("k", 4)+string(rune('a'+i))] = "v"
	}
	resp, data = postSession(t, ts.URL, map[string]any{
		"workdir": "/tmp/proj", "provider": "p", "model": "m", "ephemeral": true,
		"metadata": meta,
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "metadata") {
		t.Fatalf("oversized metadata: status %d body %s", resp.StatusCode, data)
	}
}

// configuration spec scenarios: headless 画像 / 请求级覆盖 / 未配置不变.
func TestCreateSession_DefaultAllowedToolsFallback(t *testing.T) {
	s, ts := newTestServer(t, func(c *Config) {
		c.DefaultAllowedTools = []string{"Read", "Grep", "dataweave__*"}
	})

	// No allowed_tools in the request → config default applies.
	resp, data := postSession(t, ts.URL, map[string]any{
		"workdir": "/tmp/proj", "provider": "p", "model": "m", "ephemeral": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var view sessionMeta
	_ = json.Unmarshal(data, &view)
	live, err := s.manager.GetSession(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := live.AllowedTools()
	want := map[string]bool{"Read": true, "Grep": true, "dataweave__*": true}
	if len(got) != len(want) {
		t.Fatalf("AllowedTools = %v, want default profile", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("AllowedTools = %v, want default profile", got)
		}
	}

	// Explicit allowed_tools overrides the default.
	resp, data = postSession(t, ts.URL, map[string]any{
		"workdir": "/tmp/proj", "provider": "p", "model": "m", "ephemeral": true,
		"allowed_tools": []string{"Read", "Bash"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	_ = json.Unmarshal(data, &view)
	live, _ = s.manager.GetSession(view.ID)
	got = live.AllowedTools()
	if len(got) != 2 || got[0] != "Read" || got[1] != "Bash" {
		t.Fatalf("explicit allowed_tools not honored: %v", got)
	}
}
