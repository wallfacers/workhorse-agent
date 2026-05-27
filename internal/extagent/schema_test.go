package extagent_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
)

func TestParse_ValidSubAgent(t *testing.T) {
	raw := []byte(`
name: claude-code
binary: claude
class: sub_agent
invocation:
  prompt_via: arg
  prompt_arg: --prompt
  extra_args: ["--non-interactive"]
  env_passthrough: [ANTHROPIC_API_KEY]
output:
  format: streaming-json
  stderr: separate
  parser:
    assistant_text: $.delta.text
    session_id_path: $.session_id
control:
  cancel_signal: SIGINT
  cancel_grace_sec: 5
  default_timeout_sec: 600
  max_timeout_sec: 3600
security:
  network: allowed
  filesystem: full
  trusted: true
smoke_test:
  prompt: "Reply with exactly: WORKHORSE_SMOKE_OK"
  expected_substring: WORKHORSE_SMOKE_OK
  timeout_sec: 60
description: "Claude Code agent"
usage_hints: "Use for complex coding tasks"
provenance:
  source: builtin
`)
	a, err := extagent.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Name != "claude-code" {
		t.Errorf("name: got %q", a.Name)
	}
	if a.Class != extagent.ClassSubAgent {
		t.Errorf("class: got %q", a.Class)
	}
	if a.Invocation.PromptVia != "arg" {
		t.Errorf("prompt_via: got %q", a.Invocation.PromptVia)
	}
	if a.Output.Format != "streaming-json" {
		t.Errorf("format: got %q", a.Output.Format)
	}
	if a.Output.Parser == nil || a.Output.Parser.AssistantText != "$.delta.text" {
		t.Errorf("parser.assistant_text: got %v", a.Output.Parser)
	}
	if a.Security.Trusted != true {
		t.Error("trusted should be true")
	}
	if a.Provenance.Source != "builtin" {
		t.Errorf("provenance.source: got %q", a.Provenance.Source)
	}
}

func TestParse_ValidCLITool(t *testing.T) {
	raw := []byte(`
name: pandoc-tool
binary: pandoc
class: cli_tool
description: "Document converter"
security:
  network: none
  filesystem: full
  trusted: true
provenance:
  source: user_yaml
`)
	a, err := extagent.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Class != extagent.ClassCLITool {
		t.Errorf("class: got %q", a.Class)
	}
}

func TestParse_MissingRequiredField(t *testing.T) {
	raw := []byte(`
name: test
binary: test-bin
class: sub_agent
description: "test"
security:
  network: none
  filesystem: full
  trusted: false
provenance:
  source: user_yaml
`)
	_, err := extagent.Parse(raw)
	if err == nil {
		t.Fatal("expected error for missing invocation/output/control/smoke_test")
	}
}

func TestParse_InvalidEnum(t *testing.T) {
	raw := []byte(`
name: test
binary: test-bin
class: sub_agent
invocation:
  prompt_via: arg
output:
  format: xml
control:
  cancel_signal: SIGINT
security:
  network: none
  filesystem: full
  trusted: false
smoke_test:
  prompt: "test"
  expected_substring: "test"
description: "test"
provenance:
  source: user_yaml
`)
	_, err := extagent.Parse(raw)
	if err == nil {
		t.Fatal("expected error for invalid output.format")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error should mention format: %v", err)
	}
}

func TestParse_MalformedYAML(t *testing.T) {
	raw := []byte(`
name: [invalid
  yaml: {
`)
	_, err := extagent.Parse(raw)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestParse_OutOfGrammarJSONPath(t *testing.T) {
	raw := []byte(`
name: test
binary: test
class: sub_agent
invocation:
  prompt_via: arg
output:
  format: jsonl
  parser:
    assistant_text: "$..text"
control:
  cancel_signal: SIGINT
security:
  network: none
  filesystem: full
  trusted: false
smoke_test:
  prompt: "test"
  expected_substring: "test"
description: "test"
provenance:
  source: user_yaml
`)
	_, err := extagent.Parse(raw)
	if err == nil {
		t.Fatal("expected error for recursive descent JSONPath")
	}
}

func TestParse_SubAgentRequiresInvocation(t *testing.T) {
	raw := []byte(`
name: test
binary: test
class: sub_agent
output:
  format: text
control:
  cancel_signal: SIGINT
security:
  network: none
  filesystem: full
  trusted: false
smoke_test:
  prompt: "test"
  expected_substring: "test"
description: "test"
provenance:
  source: user_yaml
`)
	_, err := extagent.Parse(raw)
	if err == nil {
		t.Fatal("expected error: sub_agent missing invocation")
	}
}

func TestParse_ValidJSONPath(t *testing.T) {
	raw := []byte(`
name: test
binary: test
class: sub_agent
invocation:
  prompt_via: arg
output:
  format: jsonl
  parser:
    assistant_text: "$.delta.text"
    session_id_path: "$.choices[*].message.content"
control:
  cancel_signal: SIGINT
security:
  network: none
  filesystem: full
  trusted: false
smoke_test:
  prompt: "test"
  expected_substring: "test"
description: "test"
provenance:
  source: user_yaml
`)
	a, err := extagent.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Output.Parser.AssistantText != "$.delta.text" {
		t.Errorf("assistant_text: got %q", a.Output.Parser.AssistantText)
	}
}

func TestAdapter_IsHealthy(t *testing.T) {
	a := &extagent.Adapter{BinaryMissing: true}
	if a.IsHealthy() {
		t.Error("adapter with missing binary should not be healthy")
	}
	a.BinaryMissing = false
	if a.IsHealthy() {
		t.Error("adapter without smoke passed should not be healthy")
	}
	a.SmokePassed = true
	if !a.IsHealthy() {
		t.Error("adapter with binary and smoke passed should be healthy")
	}
}
