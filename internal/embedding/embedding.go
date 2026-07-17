// Package embedding provides a pure-Go client for OpenAI-compatible
// /v1/embeddings endpoints (memory-hybrid-retrieval-locomo). It has no CGO and
// no third-party vector or ML dependencies; the default deployment target is a
// local Ollama instance serving a Chinese-friendly model such as
// qwen3-embedding, but any endpoint speaking the same protocol works.
//
// The client is intentionally minimal: batch embed, optional output-dimension
// request, and strict credential hygiene (the API key never appears in errors
// or logs). Callers that receive a nil Client treat semantic retrieval as
// disabled and fall back to keyword/entity signals.
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

// Client embeds batches of text into float32 vectors.
type Client interface {
	// Embed returns one vector per input text, in input order. A nil or empty
	// input returns a nil slice and no error.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model reports the configured model id, used to tag stored vectors so a
	// model change can be detected and the vectors rebuilt.
	Model() string
}

// Config parameterizes the HTTP client. BaseURL and Model must both be non-empty
// for New to return a usable client.
type Config struct {
	BaseURL    string
	Model      string
	APIKey     string
	Dimensions int
	Timeout    time.Duration
}

// HTTPClient is the concrete OpenAI-compatible implementation.
type HTTPClient struct {
	cfg  Config
	http *http.Client
}

// New builds an HTTPClient. It returns (nil, nil) when BaseURL or Model is empty
// — the documented "embedding disabled" state — so callers can write
// `c, _ := embedding.New(cfg); if c == nil { /* degrade */ }`.
func New(cfg Config) (*HTTPClient, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &HTTPClient{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Model returns the configured model id.
func (c *HTTPClient) Model() string { return c.cfg.Model }

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed calls POST {base_url}/embeddings. Errors never include the API key.
func (c *HTTPClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{
		Model:      c.cfg.Model,
		Input:      texts,
		Dimensions: c.cfg.Dimensions,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// http.Client errors can embed the URL but never the Authorization
		// header, so this is key-safe.
		return nil, fmt.Errorf("embedding: request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedding: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := "endpoint returned non-200"
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		return nil, fmt.Errorf("embedding: status %d: %s", resp.StatusCode, msg)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: expected %d vectors, got %d", len(texts), len(out.Data))
	}
	vectors := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return nil, fmt.Errorf("embedding: response index %d out of range", d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	for i, v := range vectors {
		if v == nil {
			return nil, fmt.Errorf("embedding: missing vector for input %d", i)
		}
	}
	return vectors, nil
}
