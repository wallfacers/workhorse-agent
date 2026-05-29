package judge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
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

	// Exhausted turns should return fallback.
	ch2, _ := rp.Stream(context.Background(), provider.Request{})
	var fallback []provider.ProviderEvent
	for ev := range ch2 {
		fallback = append(fallback, ev)
	}
	if len(fallback) != 1 || fallback[0].StopReason != "end_turn" {
		t.Errorf("exhausted fallback = %v, want single end_turn", fallback)
	}
}

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

	path := filepath.Join(dir, testID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("recording file is empty")
	}
	lines := splitLines(string(data))
	if len(lines) < 2 {
		t.Fatalf("expected >=2 lines, got %d", len(lines))
	}
	// Header should have test name.
	if !contains(lines[0], `"test":"test_record"`) {
		t.Errorf("header missing test name: %s", lines[0])
	}
	// Turn line should have the recorded event.
	if !contains(lines[1], `"TextDelta":"world"`) {
		t.Errorf("turn missing event: %s", lines[1])
	}
}

func TestRecordingProvider_LoadNotExist(t *testing.T) {
	rp := NewRecordingProvider(nil, ModeReplay, t.TempDir(), "no_such_test")
	if err := rp.Load(); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
