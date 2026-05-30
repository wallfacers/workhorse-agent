//go:build real_e2e

package real_e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkflow_GrepReadWrite_Full(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Search all .txt files for the word 'secret', read the file that contains it, and write the full line to a file called findings.txt.",
		Rubric:      complexWorkflowRubric,
		Timeout:     120 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "public.txt"), []byte("hello world\nnothing here\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "private.txt"), []byte("the secret key is abc123\nanother line\n"), 0o644)
			os.WriteFile(filepath.Join(workdir, "notes.txt"), []byte("meeting notes\nno secrets discussed\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestWorkflow_TodoWithTools_Full(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Create a task list: 1) Read hello.txt, 2) Count the words, 3) Write the count to count.txt. Then execute each task in order, updating the task list as you go.",
		Rubric:      complexWorkflowRubric,
		Timeout:     120 * time.Second,
		Setup: func(workdir string) {
			os.WriteFile(filepath.Join(workdir, "hello.txt"), []byte("The quick brown fox jumps over the lazy dog.\nA second line of text here.\n"), 0o644)
		},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
