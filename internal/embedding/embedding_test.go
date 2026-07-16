package embedding_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/embedding"
)

func TestNew_DisabledWhenUnconfigured(t *testing.T) {
	for _, cfg := range []embedding.Config{
		{BaseURL: "", Model: "m"},
		{BaseURL: "http://x", Model: ""},
	} {
		c, err := embedding.New(cfg)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if c != nil {
			t.Fatalf("expected nil client for cfg %+v", cfg)
		}
	}
}

func TestEmbed_RoundTripAndOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("authorization header: got %q", got)
		}
		var req struct {
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Dimensions != 4 {
			t.Errorf("dimensions passthrough: got %d", req.Dimensions)
		}
		// Return in reversed order to prove the client re-orders by Index.
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, len(req.Input))
		for i := range req.Input {
			items[len(req.Input)-1-i] = item{Embedding: []float32{float32(i), 0, 0, 0}, Index: i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer srv.Close()

	c, err := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "m", APIKey: "secret-key", Dimensions: 4})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	vecs, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	for i := range vecs {
		if vecs[i][0] != float32(i) {
			t.Fatalf("vector %d misordered: %v", i, vecs[i])
		}
	}
}

func TestEmbed_ErrorNeverLeaksKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "bad key"}})
	}))
	defer srv.Close()

	c, _ := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "m", APIKey: "super-secret"})
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked api key: %v", err)
	}
}

func TestEmbed_EmptyInput(t *testing.T) {
	c, _ := embedding.New(embedding.Config{BaseURL: "http://unused", Model: "m"})
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Fatalf("empty input: vecs=%v err=%v", vecs, err)
	}
}

func TestEmbed_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"embedding":[1,0],"index":0}]}`)
	}))
	defer srv.Close()
	c, _ := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "m"})
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected count-mismatch error")
	}
}
