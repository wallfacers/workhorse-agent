package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultJudgeModel = "glm-5"

type GLM5Judge struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
	cache   string
}

func NewGLM5Judge(opts ...func(*GLM5Judge)) *GLM5Judge {
	j := &GLM5Judge{
		apiKey:  os.Getenv("DASHSCOPE_API_KEY"),
		baseURL: os.Getenv("DASHSCOPE_BASE_URL"),
		model:   defaultJudgeModel,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	if j.baseURL == "" {
		j.baseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic"
	}
	for _, o := range opts {
		o(j)
	}
	return j
}

func WithCacheDir(dir string) func(*GLM5Judge) {
	return func(j *GLM5Judge) { j.cache = dir }
}

func WithHTTPClient(c *http.Client) func(*GLM5Judge) {
	return func(j *GLM5Judge) { j.client = c }
}

func (j *GLM5Judge) Evaluate(ctx context.Context, trace *Trace, rubric Rubric) (*JudgeResult, error) {
	if j.cache != "" {
		key := judgeCacheKey(trace, rubric)
		if cached, err := loadCachedJudge(j.cache, key); err == nil && cached != nil {
			return cached, nil
		}
	}

	prompt := j.buildPrompt(trace, rubric)
	result, err := j.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}

	if j.cache != "" {
		key := judgeCacheKey(trace, rubric)
		_ = saveCachedJudge(j.cache, key, result)
	}
	return result, nil
}

func (j *GLM5Judge) buildPrompt(trace *Trace, rubric Rubric) string {
	var sb strings.Builder
	sb.WriteString("You are a test evaluator. Evaluate the following AI agent interaction against the given criteria.\n\n")
	sb.WriteString("## User Message\n")
	sb.WriteString(trace.UserMessage + "\n\n")
	sb.WriteString("## Interaction Trace\n")
	for i, turn := range trace.Turns {
		sb.WriteString(fmt.Sprintf("### Turn %d\n", i+1))
		if turn.ModelOutput != "" {
			sb.WriteString("Model output: " + turn.ModelOutput + "\n")
		}
		for _, tc := range turn.ToolCalls {
			sb.WriteString(fmt.Sprintf("Tool call: %s(%s)\n", tc.ToolName, string(tc.Input)))
		}
		for _, tr := range turn.ToolResults {
			label := "ok"
			if tr.IsError {
				label = "error"
			}
			sb.WriteString(fmt.Sprintf("Tool result [%s]: %s\n", label, tr.Output))
		}
	}
	sb.WriteString("\n## Evaluation Criteria\n")
	for _, c := range rubric.Criteria {
		req := ""
		if c.Required {
			req = " [REQUIRED]"
		}
		sb.WriteString(fmt.Sprintf("- %s%s (weight %.2f): %s\n", c.Name, req, c.Weight, c.Description))
	}
	sb.WriteString(fmt.Sprintf("\nMinimum passing score: %.2f\n", rubric.MinScore))
	sb.WriteString("\nRespond with ONLY a JSON object:\n")
	sb.WriteString(`{"verdict":"PASS|FAIL|PARTIAL","score":0.0-1.0,"reasoning":"...","suggestions":["..."]}` + "\n")
	return sb.String()
}

func (j *GLM5Judge) callLLM(ctx context.Context, prompt string) (*JudgeResult, error) {
	body := map[string]any{
		"model":      j.model,
		"max_tokens": 1024,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	b, _ := json.Marshal(body)
	url := strings.TrimRight(j.baseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", j.apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("judge: HTTP call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("judge: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("judge: decode response: %w", err)
	}
	if len(result.Content) == 0 {
		return nil, fmt.Errorf("judge: empty response")
	}

	text := result.Content[0].Text
	jsonStart := strings.Index(text, "{")
	jsonEnd := strings.LastIndex(text, "}")
	if jsonStart == -1 || jsonEnd == -1 {
		return nil, fmt.Errorf("judge: no JSON in response: %s", text)
	}

	var jr JudgeResult
	if err := json.Unmarshal([]byte(text[jsonStart:jsonEnd+1]), &jr); err != nil {
		return nil, fmt.Errorf("judge: parse JSON: %w\nraw: %s", err, text)
	}
	return &jr, nil
}
