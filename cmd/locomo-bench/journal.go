package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// result is one graded question outcome, persisted as a JSONL line for resume.
// It deliberately never carries any credential — only benchmark content.
type result struct {
	Conv      int    `json:"conv"`
	Q         int    `json:"q"`
	Category  int    `json:"category"`
	Correct   bool   `json:"correct"`
	Question  string `json:"question"`
	Gold      string `json:"gold"`
	Predicted string `json:"predicted"`
}

type resultKey struct {
	Conv int
	Q    int
}

// journal is an append-only JSONL writer with a prior-run index for resume.
// Safe for concurrent writers (conversations and questions run in parallel).
type journal struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	seen map[resultKey]result
}

// openJournal opens (creating if needed) the run's JSONL file for the given
// retrieval mode, preloading any prior results for resume.
func openJournal(runDir, retrieval string) (*journal, error) {
	path := filepath.Join(runDir, fmt.Sprintf("results-%s.jsonl", retrieval))
	seen, err := loadPrior(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	return &journal{f: f, w: bufio.NewWriter(f), seen: seen}, nil
}

func loadPrior(path string) (map[resultKey]result, error) {
	seen := map[resultKey]result{}
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return seen, nil
		}
		return nil, fmt.Errorf("read prior journal: %w", err)
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // tolerate a partial trailing line from an interrupted run
		}
		seen[resultKey{Conv: r.Conv, Q: r.Q}] = r
	}
	return seen, sc.Err()
}

func (j *journal) lookup(k resultKey) (result, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.seen[k]
	return r, ok
}

func (j *journal) write(r result) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, _ = j.w.Write(b)
	_ = j.w.WriteByte('\n')
	_ = j.w.Flush() // flush each line so an interrupted run resumes cleanly
	j.seen[resultKey{Conv: r.Conv, Q: r.Q}] = r
}

func (j *journal) Close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.w.Flush()
	_ = j.f.Close()
}
