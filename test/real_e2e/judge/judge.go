package judge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Verdict string

const (
	VerdictPass    Verdict = "PASS"
	VerdictFail    Verdict = "FAIL"
	VerdictPartial Verdict = "PARTIAL"
)

type JudgeResult struct {
	Verdict     Verdict  `json:"verdict"`
	Score       float64  `json:"score"`
	Reasoning   string   `json:"reasoning"`
	Suggestions []string `json:"suggestions"`
}

type Rubric struct {
	Criteria   []Criterion `json:"criteria"`
	MinScore   float64     `json:"min_score"`
	MaxRetries int         `json:"max_retries"`
}

type Criterion struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight"`
	Required    bool    `json:"required"`
}

type Judge interface {
	Evaluate(ctx context.Context, trace *Trace, rubric Rubric) (*JudgeResult, error)
}

func judgeCacheKey(trace *Trace, rubric Rubric) string {
	h := sha256.New()
	json.NewEncoder(h).Encode(trace)
	json.NewEncoder(h).Encode(rubric)
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func loadCachedJudge(dir, key string) (*JudgeResult, error) {
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return nil, nil
	}
	var result JudgeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func saveCachedJudge(dir, key string, result *JudgeResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return os.WriteFile(filepath.Join(dir, key+".json"), data, 0o644)
}
