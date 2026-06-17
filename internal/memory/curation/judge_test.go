package curation

import (
	"context"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

func TestParseJudgeDecisionPlainJSON(t *testing.T) {
	raw := `{"evict":["a","b"],"merge":[{"names":["x","y"],"into":{"name":"x","trigger":"t","content":"c","durability":"volatile","category":"project"}}]}`
	d, err := parseJudgeDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(d.Evict) != 2 || d.Evict[0] != "a" {
		t.Fatalf("evict = %v", d.Evict)
	}
	if len(d.Merge) != 1 || d.Merge[0].Into.Name != "x" || len(d.Merge[0].Names) != 2 {
		t.Fatalf("merge = %+v", d.Merge)
	}
}

func TestParseJudgeDecisionToleratesFencesAndProse(t *testing.T) {
	raw := "Here is my decision:\n```json\n{\"evict\":[\"a\"],\"merge\":[]}\n```\nThanks!"
	d, err := parseJudgeDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(d.Evict) != 1 || d.Evict[0] != "a" {
		t.Fatalf("evict = %v", d.Evict)
	}
}

func TestParseJudgeDecisionNoJSON(t *testing.T) {
	if _, err := parseJudgeDecision("I could not decide."); err == nil {
		t.Fatal("expected error for prose-only response")
	}
}

func TestJudgeUsesCallerAndParses(t *testing.T) {
	var gotSystem, gotUser string
	call := func(ctx context.Context, system, user string) (string, error) {
		gotSystem, gotUser = system, user
		return `{"evict":["stale"],"merge":[]}`, nil
	}
	cands := []prompt.CurationJudgeCandidate{{Name: "stale", Trigger: "old", Content: "x"}}
	d, err := Judge(context.Background(), call, cands, nil)
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if gotSystem != prompt.CurationJudgeSystemPrompt {
		t.Fatal("judge did not pass the system prompt")
	}
	if gotUser == "" || d.Evict[0] != "stale" {
		t.Fatalf("unexpected: user=%q decision=%+v", gotUser, d)
	}
}
