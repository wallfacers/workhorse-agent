package prompt_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

func renderAdapterGeneration(t *testing.T, data map[string]any) string {
	t.Helper()
	out, err := prompt.AdapterGeneration.Execute(data)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func baseData() map[string]any {
	return map[string]any{
		"SchemaJSON":      `{"type":"object"}`,
		"BinaryName":      "gemini",
		"BinaryPath":      "/usr/local/bin/gemini",
		"HelpOutput":      "Usage: gemini [options]\n  --prompt TEXT  Send prompt and exit.",
		"VersionOutput":   "gemini 0.4.1",
		"ManOutput":       "",
		"ReadmeOutput":    "",
		"DescriptionHint": "",
		"Examples":        []prompt.AdapterGenerationExample{},
	}
}

func TestAdapterGeneration_ByteStable(t *testing.T) {
	a := renderAdapterGeneration(t, baseData())
	b := renderAdapterGeneration(t, baseData())
	if a != b {
		t.Errorf("rendered output drifted between calls — prompt caching would break")
	}
}

func TestAdapterGeneration_OmitsEmptyOptionals(t *testing.T) {
	got := renderAdapterGeneration(t, baseData())
	if strings.Contains(got, "## `man` page excerpt") {
		t.Error("empty ManOutput should suppress the man-page section")
	}
	if strings.Contains(got, "## README excerpt") {
		t.Error("empty ReadmeOutput should suppress the README section")
	}
	if strings.Contains(got, "User-provided description hint") {
		t.Error("empty DescriptionHint should suppress the hint line")
	}
}

func TestAdapterGeneration_IncludesNonEmptyOptionals(t *testing.T) {
	d := baseData()
	d["ManOutput"] = "MAN PAGE EXAMPLE"
	d["ReadmeOutput"] = "README EXAMPLE"
	d["DescriptionHint"] = "set up gemini for chat"
	got := renderAdapterGeneration(t, d)
	for _, want := range []string{
		"## `man` page excerpt",
		"MAN PAGE EXAMPLE",
		"## README excerpt",
		"README EXAMPLE",
		"set up gemini for chat",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in rendered output", want)
		}
	}
}

func TestAdapterGeneration_DisclaimerAlwaysPresent(t *testing.T) {
	// With Examples empty.
	got := renderAdapterGeneration(t, baseData())
	if !strings.Contains(got, "always prefer behavior observed in the actual --help output") {
		t.Error("disclaimer must be present even when no examples are included")
	}
	// With Examples populated.
	d := baseData()
	d["Examples"] = []prompt.AdapterGenerationExample{
		{Name: "claude-code", Body: "name: claude-code\nbinary: claude\n"},
	}
	got = renderAdapterGeneration(t, d)
	if !strings.Contains(got, "always prefer behavior observed in the actual --help output") {
		t.Error("disclaimer must be present when examples are included")
	}
	if !strings.Contains(got, "## Example: claude-code") {
		t.Error("example header should appear")
	}
	if !strings.Contains(got, "name: claude-code") {
		t.Error("example body should appear")
	}
}

func TestAdapterGeneration_EmptyExamplesShowsPlaceholder(t *testing.T) {
	got := renderAdapterGeneration(t, baseData())
	if !strings.Contains(got, "(No examples bundled in this build.)") {
		t.Error("empty Examples should produce the explicit placeholder")
	}
}

func TestAdapterGeneration_EmptyVersionOmitsSection(t *testing.T) {
	d := baseData()
	d["VersionOutput"] = ""
	got := renderAdapterGeneration(t, d)
	if strings.Contains(got, "Captured `--version`") {
		t.Error("empty VersionOutput should suppress the --version section")
	}
	if !strings.Contains(got, "Captured `--help`") {
		t.Error("--help section should still be present")
	}
}

func TestAdapterGeneration_CallToolInstructionsPresent(t *testing.T) {
	got := renderAdapterGeneration(t, baseData())
	for _, want := range []string{
		"call WriteAdapterDraft",
		"prompt_via",
		"CLI_TOOL_REFUSAL",
		"provenance.source",
		"llm_generated",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in rendered output", want)
		}
	}
}

func TestAdapterGeneration_NameInterpolatedIntoDraftPath(t *testing.T) {
	d := baseData()
	d["BinaryName"] = "my-fancy-tool"
	got := renderAdapterGeneration(t, d)
	if !strings.Contains(got, "<externalAgentsDir>/.drafts/my-fancy-tool.yaml") {
		t.Error("BinaryName should be templated into the draft path instruction")
	}
}
