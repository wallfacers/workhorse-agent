package skills_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/skills"
)

func TestFormatManifest_TwoSkills(t *testing.T) {
	cat := &skills.Catalog{
		Skills: []skills.Skill{
			{Name: "alpha", Trigger: "run alpha"},
			{Name: "beta", Trigger: "run beta"},
		},
	}
	got := skills.FormatManifest(cat)
	if !strings.Contains(got, "<available_skills>") {
		t.Error("missing <available_skills>")
	}
	if !strings.Contains(got, "</available_skills>") {
		t.Error("missing </available_skills>")
	}
	if !strings.Contains(got, "- name: alpha") {
		t.Error("missing alpha")
	}
	if !strings.Contains(got, "- name: beta") {
		t.Error("missing beta")
	}
	if !strings.Contains(got, "LoadSkill") {
		t.Error("missing LoadSkill hint")
	}
}

func TestFormatManifest_Empty(t *testing.T) {
	got := skills.FormatManifest(&skills.Catalog{})
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
	got = skills.FormatManifest(nil)
	if got != "" {
		t.Errorf("expected empty string for nil, got %q", got)
	}
}

func TestFormatManifest_SingleSkill(t *testing.T) {
	cat := &skills.Catalog{
		Skills: []skills.Skill{
			{Name: "solo", Trigger: "do the thing"},
		},
	}
	got := skills.FormatManifest(cat)
	if !strings.Contains(got, "<available_skills>") || !strings.Contains(got, "</available_skills>") {
		t.Error("missing skill tags")
	}
	if !strings.Contains(got, "- name: solo") {
		t.Error("missing skill name")
	}
}

func TestFormatManifest_TriggerWithNewlines(t *testing.T) {
	cat := &skills.Catalog{
		Skills: []skills.Skill{
			{Name: "nl", Trigger: "line1\nline2\nline3"},
		},
	}
	got := skills.FormatManifest(cat)
	if !strings.Contains(got, "trigger: line1 line2 line3") {
		t.Errorf("newlines not collapsed, got %q", got)
	}
}
