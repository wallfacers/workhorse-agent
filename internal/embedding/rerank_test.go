package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRerankerDisabled(t *testing.T) {
	for _, cfg := range []RerankConfig{
		{},
		{BaseURL: "http://x"},
		{Model: "m"},
	} {
		rr, err := NewReranker(cfg)
		if err != nil || rr != nil {
			t.Fatalf("expected (nil, nil) for %+v, got (%v, %v)", cfg, rr, err)
		}
	}
}

func TestRerankRoundTrip(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var req rerankRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Query != "q" || len(req.Documents) != 3 || req.TopN != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		// Reverse order, best-first, honoring top_n.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 2, "relevance_score": 0.9},
				{"index": 0, "relevance_score": 0.4},
			},
		})
	}))
	defer srv.Close()

	rr, err := NewReranker(RerankConfig{BaseURL: srv.URL + "/v1/", Model: "m", APIKey: "sk-secret"})
	if err != nil || rr == nil {
		t.Fatalf("new: %v %v", rr, err)
	}
	got, err := rr.Rerank(context.Background(), "q", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if gotPath != "/v1/rerank" {
		t.Fatalf("expected /v1/rerank, got %s", gotPath)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("expected bearer auth, got %q", gotAuth)
	}
	if len(got) != 2 || got[0].Index != 2 || got[0].Score != 0.9 || got[1].Index != 0 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRerankErrorNeverLeaksKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
	}))
	defer srv.Close()

	rr, _ := NewReranker(RerankConfig{BaseURL: srv.URL, Model: "m", APIKey: "sk-verysecret"})
	_, err := rr.Rerank(context.Background(), "q", []string{"a"}, 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "sk-verysecret") {
		t.Fatalf("error leaks API key: %v", err)
	}
}

func TestRerankIndexOutOfRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 7, "relevance_score": 0.9}},
		})
	}))
	defer srv.Close()

	rr, _ := NewReranker(RerankConfig{BaseURL: srv.URL, Model: "m"})
	if _, err := rr.Rerank(context.Background(), "q", []string{"a"}, 1); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestRerankEmptyInput(t *testing.T) {
	rr, _ := NewReranker(RerankConfig{BaseURL: "http://127.0.0.1:1", Model: "m"})
	got, err := rr.Rerank(context.Background(), "q", nil, 5)
	if err != nil || got != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", got, err)
	}
}
