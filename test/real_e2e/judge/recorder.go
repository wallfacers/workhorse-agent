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

func ModeFromEnv() RecordMode {
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
	Request provider.Request        `json:"request"`
	Events  []provider.ProviderEvent `json:"events"`
}

type RecordingProvider struct {
	inner  provider.Provider
	mode   RecordMode
	dir    string
	testID string

	mu     sync.Mutex
	file   *os.File
	turns  []recordedTurn
	offset int
}

func NewRecordingProvider(inner provider.Provider, mode RecordMode, dir, testID string) *RecordingProvider {
	return &RecordingProvider{
		inner:  inner,
		mode:   mode,
		dir:    dir,
		testID: testID,
	}
}

func (rp *RecordingProvider) Name() string {
	if rp.inner != nil {
		return rp.inner.Name()
	}
	return "recording-provider"
}

func (rp *RecordingProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.ProviderEvent, error) {
	switch rp.mode {
	case ModeReplay:
		return rp.streamReplay(ctx)
	case ModeRecord:
		return rp.streamRecord(ctx, req)
	case ModeLive:
		return rp.inner.Stream(ctx, req)
	default:
		return rp.inner.Stream(ctx, req)
	}
}

func (rp *RecordingProvider) Load() error {
	path := filepath.Join(rp.dir, rp.testID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("recorder: read %s: %w", path, err)
	}
	lines := splitLines(string(data))
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

func (rp *RecordingProvider) Model() string {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	path := filepath.Join(rp.dir, rp.testID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := splitLines(string(data))
	if len(lines) == 0 {
		return ""
	}
	var hdr recordingHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		return ""
	}
	return hdr.Model
}

func (rp *RecordingProvider) streamReplay(ctx context.Context) (<-chan provider.ProviderEvent, error) {
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

func (rp *RecordingProvider) Save() error {
	if err := rp.flush(); err != nil {
		return err
	}
	if rp.file != nil {
		rp.file.Close()
		rp.file = nil
	}
	return nil
}

func (rp *RecordingProvider) flush() error {
	if len(rp.turns) == 0 {
		return nil
	}
	if rp.file == nil {
		if err := os.MkdirAll(rp.dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(rp.dir, rp.testID+".jsonl")
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		rp.file = f
		hdr := recordingHeader{Test: rp.testID, Model: "recorded", RecordedAt: time.Now()}
		hdrBytes, _ := json.Marshal(hdr)
		if _, err := f.WriteString(string(hdrBytes) + "\n"); err != nil {
			return err
		}
	}
	for _, turn := range rp.turns {
		b, _ := json.Marshal(turn)
		if _, err := rp.file.WriteString(string(b) + "\n"); err != nil {
			return err
		}
	}
	rp.turns = nil
	return nil
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
