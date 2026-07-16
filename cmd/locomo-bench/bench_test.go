package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestParseLoCoMoDate(t *testing.T) {
	cases := map[string]bool{ // input → expect non-zero
		"1:56 pm on 8 May, 2023":  true,
		"7:00 pm on 25 May, 2023": true,
		"8 May, 2023":             true,
		"":                        false,
		"garbage":                 false,
	}
	for in, wantOK := range cases {
		got := parseLoCoMoDate(in)
		if got.IsZero() == wantOK {
			t.Errorf("parseLoCoMoDate(%q) = %v (zero=%v), want non-zero=%v", in, got, got.IsZero(), wantOK)
		}
	}
	// Spot-check a parsed value.
	if d := parseLoCoMoDate("1:56 pm on 8 May, 2023"); d.Year() != 2023 || d.Month() != time.May || d.Day() != 8 {
		t.Errorf("date fields wrong: %v", d)
	}
}

func TestParseJudgeVerdict(t *testing.T) {
	cases := map[string]bool{
		`{"correct": true}`:                        true,
		`{"correct":false}`:                        false,
		"The verdict is correct: true.":            true,
		"correct is false because it contradicts":  false,
		"no verdict token here":                    false,
		`{"correct": true, "note":"ignore false"}`: true,
	}
	for in, want := range cases {
		if got := parseJudgeVerdict(in); got != want {
			t.Errorf("parseJudgeVerdict(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAggregatorAndPct(t *testing.T) {
	a := newAggregator()
	a.add(4, true)
	a.add(4, false)
	a.add(1, true)
	if a.byCategory[4].total != 2 || a.byCategory[4].correct != 1 {
		t.Fatalf("cat 4 stats wrong: %+v", a.byCategory[4])
	}
	if pct(1, 2) != 50 {
		t.Fatalf("pct(1,2)=%v", pct(1, 2))
	}
	if pct(0, 0) != 0 {
		t.Fatalf("pct(0,0) should be 0")
	}
}

func TestRetrievedMemoryLine(t *testing.T) {
	m := retrievedMemory{Content: "moved to Berlin", EventDate: "2019-05-01", Recorded: "2026-07-16"}
	got := m.Line()
	want := "[event: 2019-05-01] [recorded: 2026-07-16] moved to Berlin"
	if got != want {
		t.Fatalf("Line() = %q, want %q", got, want)
	}
}

func TestJournalResume(t *testing.T) {
	dir := t.TempDir()
	j, err := openJournal(dir, "hybrid")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	j.write(result{Conv: 0, Q: 3, Category: 4, Correct: true, Question: "q", Gold: "g", Predicted: "p"})
	j.Close()

	// Reopen: prior result must be visible for resume.
	j2, err := openJournal(dir, "hybrid")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer j2.Close()
	r, ok := j2.lookup(resultKey{Conv: 0, Q: 3})
	if !ok || !r.Correct {
		t.Fatalf("resume lookup failed: r=%+v ok=%v", r, ok)
	}
	// A different retrieval mode has its own file (no cross-contamination).
	if _, err := filepath.Glob(filepath.Join(dir, "results-*.jsonl")); err != nil {
		t.Fatalf("glob: %v", err)
	}
}

func TestArmsFor(t *testing.T) {
	cases := map[string][]string{
		"fts":    {"fts"},
		"hybrid": {"hybrid"},
		"both":   {"fts", "hybrid"},
	}
	for in, want := range cases {
		got, err := armsFor(in)
		if err != nil {
			t.Fatalf("armsFor(%q) err: %v", in, err)
		}
		if len(got) != len(want) {
			t.Fatalf("armsFor(%q) = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("armsFor(%q) = %v, want %v", in, got, want)
			}
		}
	}
	if _, err := armsFor("bogus"); err == nil {
		t.Fatal("armsFor(bogus) should error")
	}
	if !hasArm([]string{"fts", "hybrid"}, "hybrid") || hasArm([]string{"fts"}, "hybrid") {
		t.Fatal("hasArm wrong")
	}
}

func TestGateBoundsConcurrency(t *testing.T) {
	sem := make(chan struct{}, 2)
	var mu sync.Mutex
	inflight, peak := 0, 0
	base := func(ctx context.Context, _, _ string) (string, error) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return "ok", nil
	}
	gated := gate(sem, base)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = gated(context.Background(), "", "") }()
	}
	wg.Wait()
	if peak > 2 {
		t.Fatalf("gate allowed %d concurrent, cap was 2", peak)
	}
}

func TestParseDataset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mini.json")
	data := `[
	  {
	    "qa": [
	      {"question":"Where did the user move from?","answer":"Sweden","category":4},
	      {"question":"adversarial one","answer":"n/a","category":5}
	    ],
	    "conversation": {
	      "speaker_a":"Alex","speaker_b":"Sam",
	      "session_1_date_time":"1:56 pm on 8 May, 2023",
	      "session_1":[
	        {"speaker":"Alex","text":"I moved from Sweden."},
	        {"speaker":"Sam","text":"Nice."}
	      ]
	    }
	  }
	]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	convs, err := loadDataset(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(convs))
	}
	c := convs[0]
	if len(c.Sessions) != 1 || len(c.Sessions[0].Turns) != 2 {
		t.Fatalf("session parse wrong: %+v", c.Sessions)
	}
	if c.Sessions[0].Date.Year() != 2023 {
		t.Fatalf("session date not parsed: %v", c.Sessions[0].Date)
	}
	if len(c.QA) != 2 || c.QA[0].AnswerText() != "Sweden" {
		t.Fatalf("qa parse wrong: %+v", c.QA)
	}
}
