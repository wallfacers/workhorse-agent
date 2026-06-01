package session

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// TestContentRoundTrip asserts that a message carrying every ContentBlock kind
// survives marshalContent → unmarshalContent unchanged. The thinking block's
// Signature is the load-bearing field: it must persist so a hydrated session's
// API round-trip stays valid (see add-project-sessions design D9).
func TestContentRoundTrip(t *testing.T) {
	blocks := []provider.ContentBlock{
		{Type: provider.BlockText, Text: "hello 世界"},
		{Type: provider.BlockToolUse, ToolUseID: "call_1", ToolName: "Bash", Input: json.RawMessage(`{"cmd":"ls"}`)},
		{Type: provider.BlockToolResult, ToolUseID: "call_1", Output: "a\nb", IsError: true},
		{Type: provider.BlockThinking, Thinking: "let me think", Signature: "sig-abc123"},
		{Type: provider.BlockRedactedThinking, RedactedData: "redacted-blob"},
	}

	encoded, err := marshalContent(blocks)
	if err != nil {
		t.Fatalf("marshalContent: %v", err)
	}
	if encoded == "" {
		t.Fatal("marshalContent returned empty string")
	}

	got, err := unmarshalContent(encoded)
	if err != nil {
		t.Fatalf("unmarshalContent: %v", err)
	}

	if !reflect.DeepEqual(got, blocks) {
		t.Fatalf("round-trip mismatch:\n got = %#v\nwant = %#v", got, blocks)
	}
}

// TestAppendMessagePersists asserts a non-ephemeral session writes each
// appended message to the store with the serialised content_json (Group 0.2).
func TestAppendMessagePersists(t *testing.T) {
	fs := &fakeStore{}
	s := New(Options{Store: fs, Workdir: "/tmp"})
	blocks := []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}

	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleUser, Content: blocks})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.messages) != 1 {
		t.Fatalf("want 1 persisted message, got %d", len(fs.messages))
	}
	m := fs.messages[0]
	if m.SessionID != s.ID {
		t.Errorf("SessionID = %q, want %q", m.SessionID, s.ID)
	}
	if m.Role != string(provider.RoleUser) {
		t.Errorf("Role = %q, want %q", m.Role, provider.RoleUser)
	}
	if len(m.ID) != 26 {
		t.Errorf("message ID expected 26-char ULID, got %d (%q)", len(m.ID), m.ID)
	}
	if m.CreatedAt.IsZero() {
		t.Error("CreatedAt must be set")
	}
	want, _ := marshalContent(blocks)
	if m.ContentJSON != want {
		t.Errorf("ContentJSON = %q, want %q", m.ContentJSON, want)
	}
	// In-memory history is still populated regardless of persistence.
	if got := len(s.History()); got != 1 {
		t.Errorf("in-memory history len = %d, want 1", got)
	}
}

// TestAppendMessageEphemeralSkipsStore asserts ephemeral sessions never persist
// messages, but still keep them in memory.
func TestAppendMessageEphemeralSkipsStore(t *testing.T) {
	fs := &fakeStore{}
	s := New(Options{Store: fs, Workdir: "/tmp", Ephemeral: true})

	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "x"}}})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.messages) != 0 {
		t.Fatalf("ephemeral session must not persist messages, got %d", len(fs.messages))
	}
	if got := len(s.History()); got != 1 {
		t.Errorf("in-memory history len = %d, want 1", got)
	}
}

// TestReplaceHistoryPersists asserts compaction's ReplaceHistory rewrites the
// persisted transcript so the store stays equal to the in-memory context
// (Group 0.3 / design D9).
func TestReplaceHistoryPersists(t *testing.T) {
	fs := &fakeStore{}
	s := New(Options{Store: fs, Workdir: "/tmp"})
	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "one"}}})
	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "two"}}})

	summary := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "summary"}}},
	}
	s.ReplaceHistory(context.Background(), summary)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.messages) != 1 {
		t.Fatalf("after ReplaceHistory want 1 persisted message, got %d", len(fs.messages))
	}
	want, _ := marshalContent(summary[0].Content)
	if fs.messages[0].ContentJSON != want {
		t.Errorf("persisted content = %q, want %q", fs.messages[0].ContentJSON, want)
	}
	if fs.messages[0].SessionID != s.ID {
		t.Errorf("SessionID = %q, want %q", fs.messages[0].SessionID, s.ID)
	}
	if got := len(s.History()); got != 1 {
		t.Errorf("in-memory history len = %d, want 1", got)
	}
}

// TestReplaceHistoryEphemeralSkipsStore asserts ephemeral compaction does not
// touch the store.
func TestReplaceHistoryEphemeralSkipsStore(t *testing.T) {
	fs := &fakeStore{}
	s := New(Options{Store: fs, Workdir: "/tmp", Ephemeral: true})
	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "x"}}})
	s.ReplaceHistory(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "y"}}}})

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.messages) != 0 {
		t.Fatalf("ephemeral ReplaceHistory must not persist, got %d", len(fs.messages))
	}
}

// TestContentRoundTripEmpty covers a message with no blocks (defensive: a
// user_message could in theory be empty).
func TestContentRoundTripEmpty(t *testing.T) {
	encoded, err := marshalContent(nil)
	if err != nil {
		t.Fatalf("marshalContent(nil): %v", err)
	}
	got, err := unmarshalContent(encoded)
	if err != nil {
		t.Fatalf("unmarshalContent: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %#v", got)
	}
}
