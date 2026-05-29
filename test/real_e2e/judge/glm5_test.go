package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGLM5Judge_EvaluateWithMock(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("missing Anthropic-Version header")
		}
		resp := map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": `{"verdict":"PASS","score":0.85,"reasoning":"Model correctly called Read tool and reported file content.","suggestions":[]}`,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	judge := NewGLM5Judge(func(j *GLM5Judge) {
		j.baseURL = ts.URL
		j.apiKey = "test-key"
	})

	trace := &Trace{
		TestName:    "test_read",
		UserMessage: "Read the file /tmp/test.txt",
		Turns: []Turn{
			{
				ModelOutput: "The file contains: hello world",
				ToolCalls: []ToolCallRecord{
					{ToolName: "Read", Input: json.RawMessage(`{"path":"/tmp/test.txt"}`)},
				},
				ToolResults: []ToolResultRecord{
					{ToolName: "Read", Output: "hello world"},
				},
			},
		},
	}

	rubric := Rubric{
		MinScore:   0.7,
		MaxRetries: 2,
		Criteria: []Criterion{
			{Name: "tool_correct", Description: "Called Read with correct path", Weight: 0.5, Required: true},
			{Name: "output_match", Description: "Reported correct content", Weight: 0.5, Required: true},
		},
	}

	result, err := judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want PASS", result.Verdict)
	}
	if result.Score < rubric.MinScore {
		t.Errorf("score = %.2f, want >= %.2f", result.Score, rubric.MinScore)
	}
}

func TestGLM5Judge_Caching(t *testing.T) {
	cacheDir := t.TempDir()
	callCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"verdict":"PASS","score":0.9,"reasoning":"ok","suggestions":[]}`},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	judge := NewGLM5Judge(func(j *GLM5Judge) {
		j.baseURL = ts.URL
		j.apiKey = "test-key"
	}, WithCacheDir(cacheDir))

	trace := &Trace{TestName: "cache_test", UserMessage: "hi", Turns: []Turn{{ModelOutput: "hello"}}}
	rubric := Rubric{MinScore: 0.5, Criteria: []Criterion{{Name: "ok", Description: "ok", Weight: 1.0}}}

	_, err := judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 API call, got %d", callCount)
	}

	_, err = judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call (cached), got %d", callCount)
	}
}

func TestGLM5Judge_MarkdownWrappedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Here is my evaluation:\n```json\n{\"verdict\":\"FAIL\",\"score\":0.3,\"reasoning\":\"bad\",\"suggestions\":[\"fix it\"]}\n```"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	judge := NewGLM5Judge(func(j *GLM5Judge) {
		j.baseURL = ts.URL
		j.apiKey = "test-key"
	})

	trace := &Trace{TestName: "test", UserMessage: "hi", Turns: []Turn{{ModelOutput: "wrong"}}}
	rubric := Rubric{MinScore: 0.5, Criteria: []Criterion{{Name: "ok", Description: "ok", Weight: 1.0}}}

	result, err := judge.Evaluate(context.Background(), trace, rubric)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Verdict != VerdictFail {
		t.Errorf("verdict = %q, want FAIL", result.Verdict)
	}
	if len(result.Suggestions) != 1 || result.Suggestions[0] != "fix it" {
		t.Errorf("suggestions = %v, want [fix it]", result.Suggestions)
	}
}
