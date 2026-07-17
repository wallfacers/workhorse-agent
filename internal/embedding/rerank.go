package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RankedDoc is one reranked document: Index refers back to the input slice.
type RankedDoc struct {
	Index int
	Score float64
}

// Reranker scores documents against a query with a cross-encoder. Like Client,
// a nil Reranker means "reranking disabled" and callers keep their fused order.
type Reranker interface {
	// Rerank returns up to topN documents ordered by relevance descending.
	// topN <= 0 returns all inputs ranked.
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]RankedDoc, error)
	// Model reports the configured rerank model id.
	Model() string
}

// RerankConfig parameterizes the HTTP reranker. BaseURL and Model must both be
// non-empty for NewReranker to return a usable client.
type RerankConfig struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
}

// HTTPReranker speaks the de-facto standard rerank protocol
// (Cohere/Jina-compatible): POST {base_url}/rerank with
// {model, query, documents, top_n} returning {results:[{index, relevance_score}]}.
type HTTPReranker struct {
	cfg  RerankConfig
	http *http.Client
}

// NewReranker builds an HTTPReranker. It returns (nil, nil) when BaseURL or
// Model is empty — the documented "reranking disabled" state.
func NewReranker(cfg RerankConfig) (*HTTPReranker, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &HTTPReranker{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Model returns the configured rerank model id.
func (r *HTTPReranker) Model() string { return r.cfg.Model }

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Rerank calls POST {base_url}/rerank. Errors never include the API key.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, documents []string, topN int) ([]RankedDoc, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(rerankRequest{
		Model:     r.cfg.Model,
		Query:     query,
		Documents: documents,
		TopN:      topN,
	})
	if err != nil {
		return nil, fmt.Errorf("rerank: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("rerank: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := "endpoint returned non-200"
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		return nil, fmt.Errorf("rerank: status %d: %s", resp.StatusCode, msg)
	}
	ranked := make([]RankedDoc, 0, len(out.Results))
	for _, res := range out.Results {
		if res.Index < 0 || res.Index >= len(documents) {
			return nil, fmt.Errorf("rerank: response index %d out of range", res.Index)
		}
		ranked = append(ranked, RankedDoc{Index: res.Index, Score: res.RelevanceScore})
	}
	return ranked, nil
}
