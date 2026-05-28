package smoke

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/driver"
	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
)

const lockTimeout = 30 * time.Second

// SmokeResult holds the outcome of a smoke test run.
type SmokeResult struct {
	Passed     bool   `json:"passed"`
	RanAt      string `json:"ran_at"`
	OutputHash string `json:"output_hash,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Run executes the adapter's smoke test in a sandboxed environment.
func Run(adapter *extagent.Adapter, logger *slog.Logger) SmokeResult {
	sandboxDir, err := os.MkdirTemp(os.TempDir(), "workhorse-smoke-*")
	if err != nil {
		return SmokeResult{Passed: false, RanAt: time.Now().Format(time.RFC3339), Error: err.Error()}
	}
	defer os.RemoveAll(sandboxDir)

	timeout := time.Duration(adapter.SmokeTest.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Use the driver to invoke — driver.buildEnv handles env_passthrough
	// and env_override from the adapter definition.
	d := &driver.Driver{Logger: logger}
	result, err := d.Run(ctx, adapter, adapter.SmokeTest.Prompt, driver.Opts{
		SessionID:       "smoke",
		CallID:          fmt.Sprintf("smoke-%d", time.Now().UnixNano()),
		TimeoutSec:      adapter.SmokeTest.TimeoutSec,
		Workdir:         sandboxDir,
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	})
	if err != nil {
		return SmokeResult{
			Passed: false,
			RanAt:  time.Now().Format(time.RFC3339),
			Error:  fmt.Sprintf("driver error: %v", err),
		}
	}

	// Override env (driver builds its own, but we need the sandbox env).
	// The driver's env composition is correct since we've set up env_override properly.
	// Check for timeout.
	if result.TimedOut {
		return SmokeResult{Passed: false, RanAt: time.Now().Format(time.RFC3339), Error: "timeout"}
	}

	// Check for expected substring.
	output := result.Stdout
	if !strings.Contains(output, adapter.SmokeTest.ExpectedSubstring) {
		return SmokeResult{
			Passed: false,
			RanAt:  time.Now().Format(time.RFC3339),
			Error:  fmt.Sprintf("expected substring %q not found in output (first 200 chars: %s)", adapter.SmokeTest.ExpectedSubstring, truncate(output, 200)),
		}
	}

	hash := sha256.Sum256([]byte(output))
	return SmokeResult{
		Passed:     true,
		RanAt:      time.Now().Format(time.RFC3339),
		OutputHash: fmt.Sprintf("%x", hash[:8]),
	}
}

// RunCachedAll runs cached smoke tests for sub_agent adapters in the registry,
// updating each adapter's SmokePassed/SmokeError/BinaryMissing fields.
// Non-sub_agent adapters (cli_tool) are skipped — they don't declare smoke_test.
func RunCachedAll(reg *extagent.Registry, cacheDir string, cacheTTLHours int, logger *slog.Logger) {
	for _, a := range reg.Adapters() {
		if a.Class != extagent.ClassSubAgent {
			continue
		}
		if a.SmokeTest.Prompt == "" {
			continue
		}
		passed := RunCached(a, cacheDir, cacheTTLHours, logger)
		a.SmokePassed = passed
		if !passed {
			a.SmokeError = "smoke test failed"
		}
	}
}

// RunCached runs smoke test with cache support. Returns true if passed (or cached pass).
func RunCached(adapter *extagent.Adapter, cacheDir string, cacheTTLHours int, logger *slog.Logger) bool {
	smokeFile := filepath.Join(cacheDir, adapter.Name+".smoke")

	// Check cache.
	if cached := readSmokeCache(smokeFile, adapter, cacheTTLHours, logger); cached != nil {
		if cached.Passed {
			return true
		}
		return false
	}

	// Run smoke test.
	result := Run(adapter, logger)

	// Write cache.
	writeSmokeCache(smokeFile, result, logger)

	return result.Passed
}

func readSmokeCache(path string, adapter *extagent.Adapter, cacheTTLHours int, logger *slog.Logger) *SmokeResult {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sr SmokeResult
	if err := json.Unmarshal(data, &sr); err != nil {
		logger.Debug("smoke: malformed cache, re-running", "path", path)
		return nil
	}

	// Check TTL.
	if sr.RanAt != "" {
		ranAt, err := time.Parse(time.RFC3339, sr.RanAt)
		if err == nil {
			ttl := time.Duration(cacheTTLHours) * time.Hour
			if time.Since(ranAt) > ttl {
				logger.Debug("smoke: cache TTL expired", "adapter", adapter.Name)
				return nil
			}
		}
	}

	return &sr
}

func writeSmokeCache(path string, result SmokeResult, logger *slog.Logger) {
	data, err := json.Marshal(result)
	if err != nil {
		logger.Warn("smoke: failed to marshal result", "err", err)
		return
	}
	// Atomic write: temp + rename.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logger.Warn("smoke: failed to write temp", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		logger.Warn("smoke: failed to rename cache", "err", err)
	}
}

func buildSandboxEnv(adapter *extagent.Adapter) []string {
	// Start from filtered parent env.
	kept, _ := bash.Filter(os.Environ())

	// Restrict to passthrough.
	if len(adapter.Invocation.EnvPassthrough) > 0 {
		passthroughSet := make(map[string]bool, len(adapter.Invocation.EnvPassthrough))
		for _, k := range adapter.Invocation.EnvPassthrough {
			passthroughSet[k] = true
		}
		var filtered []string
		for _, e := range kept {
			k, _ := splitKV(e)
			if passthroughSet[k] {
				filtered = append(filtered, e)
			}
		}
		kept = filtered
	}

	// Layer env_override.
	envMap := make(map[string]string, len(kept))
	for _, e := range kept {
		k, v := splitKV(e)
		envMap[k] = v
	}
	for k, v := range adapter.Invocation.EnvOverride {
		envMap[k] = v
	}

	// Minimum allowlist for smoke.
	minAllow := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LANG": true}
	// Keep only minimum + overrides.
	sandbox := make(map[string]string)
	for k, v := range envMap {
		if minAllow[k] {
			sandbox[k] = v
		}
	}
	// Also keep any env_override keys.
	for k, v := range adapter.Invocation.EnvOverride {
		sandbox[k] = v
	}

	var result []string
	for k, v := range sandbox {
		result = append(result, k+"="+v)
	}
	return result
}

func splitKV(e string) (string, string) {
	i := strings.IndexByte(e, '=')
	if i < 0 {
		return e, ""
	}
	return e[:i], e[i+1:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
