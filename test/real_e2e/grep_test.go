//go:build real_e2e

package real_e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGrep_FindPattern_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Search for the word 'database' in all files under the current directory and show me the matches.",
		Rubric:      grepRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "app.go"), []byte("package main\n\nfunc connectDatabase() {\n\t// database connection\n}\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "config.yaml"), []byte("host: localhost\nport: 5432\ndatabase: mydb\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "readme.md"), []byte("# My App\n\nNo database info here.\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestGrep_WithIncludeFilter_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Search for 'ERROR' in only *.log files in the current directory and list all matches.",
		Rubric:      grepRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "app.log"), []byte("2025-01-01 INFO started\n2025-01-02 ERROR disk full\n2025-01-03 ERROR timeout\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "debug.log"), []byte("2025-01-04 ERROR null pointer\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "app.go"), []byte("// ERROR handling code\nfunc handleError() {}\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestGrep_NoMatch_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Search for the word 'xylophone_quantum_42' in all files under the current directory.",
		Rubric:      grepNoMatchRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "notes.txt"), []byte("hello world\nfoo bar baz\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "data.json"), []byte(`{"key": "value"}`), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
