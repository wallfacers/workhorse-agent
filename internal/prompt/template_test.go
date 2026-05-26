package prompt_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
	"github.com/wallfacers/workhorse-agent/internal/skills"
)

// Baseline values captured from the original agent.BuildSystemPrompt on master
// before migration. These are used for byte-level equivalence verification.
var (
	baselineEmptyBytes = "Note: if a tool_result begins with `[CANCELLED]`, " +
		"the tool call was interrupted by the user. Do not retry it automatically; " +
		"acknowledge the interruption and ask the user how to proceed."
	baselineWithBaseBytes = "You are a helper.\n\n" +
		"Note: if a tool_result begins with `[CANCELLED]`, " +
		"the tool call was interrupted by the user. Do not retry it automatically; " +
		"acknowledge the interruption and ask the user how to proceed."
)

func TestSystemPrompt_EmptyBase(t *testing.T) {
	got := prompt.BuildSystemPrompt("")
	if got != baselineEmptyBytes {
		t.Errorf("empty base mismatch:\ngot:  %q\nwant: %q", got, baselineEmptyBytes)
	}
	if strings.HasPrefix(got, "\n") {
		t.Error("empty base should not start with a newline")
	}
}

func TestSystemPrompt_WithBase(t *testing.T) {
	got := prompt.BuildSystemPrompt("You are a helper.")
	if got != baselineWithBaseBytes {
		t.Errorf("with-base mismatch:\ngot:  %q\nwant: %q", got, baselineWithBaseBytes)
	}
}

func TestSystemPrompt_TrailingWhitespace(t *testing.T) {
	got := prompt.BuildSystemPrompt("hello   \t\n")
	if !strings.HasPrefix(got, "hello\n\n") {
		t.Errorf("trailing whitespace not trimmed: %q", got)
	}
}

func TestCompaction_Render(t *testing.T) {
	out, err := prompt.Compaction.Execute(nil)
	if err != nil {
		t.Fatalf("Compaction.Execute failed: %v", err)
	}
	if !strings.Contains(out, "conversation summariser") {
		t.Errorf("compaction prompt missing key phrase: %q", out)
	}
	if !strings.Contains(out, "400 tokens") {
		t.Errorf("compaction prompt missing token limit: %q", out)
	}
}

func TestSkillManifest_Multiple(t *testing.T) {
	cat := &skills.Catalog{Skills: []skills.Skill{
		{Name: "alpha", Trigger: "run alpha"},
		{Name: "beta", Trigger: "run beta"},
	}}
	got := skills.FormatManifest(cat)
	if !strings.Contains(got, "<available_skills>") {
		t.Error("missing <available_skills>")
	}
	if !strings.Contains(got, "</available_skills>") {
		t.Error("missing </available_skills>")
	}
	if !strings.Contains(got, "- name: alpha") {
		t.Error("missing alpha name")
	}
	if !strings.Contains(got, "- name: beta") {
		t.Error("missing beta name")
	}
	if !strings.Contains(got, "trigger: run alpha") {
		t.Error("missing alpha trigger")
	}
	if !strings.Contains(got, "trigger: run beta") {
		t.Error("missing beta trigger")
	}
}

func TestSkillManifest_Single(t *testing.T) {
	cat := &skills.Catalog{Skills: []skills.Skill{
		{Name: "solo", Trigger: "do the thing"},
	}}
	got := skills.FormatManifest(cat)
	if !strings.Contains(got, "<available_skills>") || !strings.Contains(got, "</available_skills>") {
		t.Error("missing skill tags")
	}
	if !strings.Contains(got, "- name: solo") {
		t.Error("missing skill name")
	}
}

func TestSkillManifest_EmptyData(t *testing.T) {
	got := skills.FormatManifest(&skills.Catalog{})
	if got != "" {
		t.Errorf("empty catalog should return empty string, got %q", got)
	}
	got = skills.FormatManifest(nil)
	if got != "" {
		t.Errorf("nil catalog should return empty string, got %q", got)
	}
}

func TestFormatManifest_EnglishFooter(t *testing.T) {
	cat := &skills.Catalog{Skills: []skills.Skill{
		{Name: "x", Trigger: "do x"},
	}}
	got := skills.FormatManifest(cat)
	if !strings.Contains(got, "You can use the LoadSkill tool to load full instructions.") {
		t.Error("missing English footer")
	}
	if strings.Contains(got, "可以调用") {
		t.Error("should not contain Chinese text after migration")
	}
}

func TestSSTIImmunity(t *testing.T) {
	got, err := prompt.SystemPrompt.Execute(map[string]any{
		"BasePrompt": "{{evil}}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "{{evil}}") {
		t.Error("template should treat {{evil}} as a literal string, not parse it")
	}
}

func TestMustParse_InvalidPanic(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("MustParse with invalid template should panic")
		}
	}()
	prompt.MustParse("bad", "{{.Unclosed")
}

func TestBuildSystemPrompt_Equivalence(t *testing.T) {
	empty := prompt.BuildSystemPrompt("")
	if empty != baselineEmptyBytes {
		t.Errorf("BuildSystemPrompt('') not equivalent to baseline:\ngot:  %q\nwant: %q",
			empty, baselineEmptyBytes)
	}

	withBase := prompt.BuildSystemPrompt("You are a helper.")
	if withBase != baselineWithBaseBytes {
		t.Errorf("BuildSystemPrompt('You are a helper.') not equivalent to baseline:\ngot:  %q\nwant: %q",
			withBase, baselineWithBaseBytes)
	}
}

func TestSkillManifest_Golden(t *testing.T) {
	cat := &skills.Catalog{Skills: []skills.Skill{
		{Name: "alpha", Trigger: "run alpha"},
		{Name: "beta", Trigger: "run beta"},
	}}
	want := "<available_skills>\n" +
		"- name: alpha\n  trigger: run alpha\n" +
		"- name: beta\n  trigger: run beta\n" +
		"</available_skills>\n\n" +
		"You can use the LoadSkill tool to load full instructions.\n"
	got := skills.FormatManifest(cat)
	if got != want {
		t.Errorf("manifest byte mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBuildSystemPrompt_ComposedWithManifest(t *testing.T) {
	// Mirrors the cmd_serve.go wiring: operator base + "\n\n" + manifest,
	// then BuildSystemPrompt. Locks the byte output to catch concat-ordering
	// or trailing-whitespace regressions.
	manifest := skills.FormatManifest(&skills.Catalog{Skills: []skills.Skill{
		{Name: "x", Trigger: "do x"},
	}})
	composed := "Hello\n\n" + manifest
	got := prompt.BuildSystemPrompt(composed)
	want := "Hello\n\n" +
		"<available_skills>\n" +
		"- name: x\n  trigger: do x\n" +
		"</available_skills>\n\n" +
		"You can use the LoadSkill tool to load full instructions.\n\n" +
		prompt.CancelledNote
	if got != want {
		t.Errorf("composed prompt byte mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestCancelledMarkerConsistency(t *testing.T) {
	if !strings.HasPrefix(prompt.CancelledToolOutput, "[CANCELLED]") {
		t.Errorf("CancelledToolOutput should start with [CANCELLED], got %q", prompt.CancelledToolOutput)
	}
	if !strings.Contains(prompt.CancelledNote, "`[CANCELLED]`") {
		t.Errorf("CancelledNote should contain `[CANCELLED]` marker, got %q", prompt.CancelledNote)
	}
}
