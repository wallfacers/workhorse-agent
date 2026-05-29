//go:build real_e2e

package real_e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRead_Basic_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Read the file hello.txt in the current directory and tell me what it says.",
		Rubric:      fileToolsRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "hello.txt"), []byte("Hello from workhorse-agent E2E test!"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestFileRead_NotFound_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Read the file nonexistent_file.txt and tell me its contents.",
		Rubric:      fileNotFoundRubric,
		Timeout:     60 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestFileWrite_Create_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Create a file called output.txt with the content 'E2E test write'.",
		Rubric:      fileToolsRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestFileEdit_Modify_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Edit the file config.yaml: change 'debug: false' to 'debug: true'.",
		Rubric:      fileToolsRubric,
		Timeout:     90 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "config.yaml"), []byte("debug: false\nport: 8080\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestBash_ListDir_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Run 'ls -la' in the current directory and list the files you see.",
		Rubric:      fileToolsRubric,
		Timeout:     60 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "a.txt"), []byte("aaa"), 0o644)
			os.WriteFile(filepath.Join(workdir, "b.txt"), []byte("bbb"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestMultiTool_Workflow_Full(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Read the file data.csv, find the highest value in the second column, and write it to max_value.txt.",
		Rubric:      fileToolsRubric,
		Timeout:     120 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "data.csv"), []byte("name,value\nalpha,10\nbeta,42\ngamma,7\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
