package skills_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/skills"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

type allowedToolsRecorder struct {
	tools []string
}

func (r *allowedToolsRecorder) SetAllowedTools(t []string)     { r.tools = t }
func (r *allowedToolsRecorder) MarkToolsDiscovered(_ []string) {}

func catalogWithSkills() *skills.Catalog {
	return &skills.Catalog{
		Skills: []skills.Skill{
			{Name: "git-helper", Content: "Use git add, commit, push.\n"},
			{Name: "restricted", Content: "Limited skill.\n", AllowedTools: []string{"Read", "Grep"}},
		},
	}
}

func TestLoadSkill_Happy(t *testing.T) {
	ls := skills.NewLoadSkill(catalogWithSkills())
	res, err := ls.Run(context.Background(), &tools.Env{}, json.RawMessage(`{"name":"git-helper"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError")
	}
	if res.Output != "Use git add, commit, push.\n" {
		t.Fatalf("unexpected output: %q", res.Output)
	}
	if res.Modifier != nil {
		t.Fatalf("expected no modifier for unrestricted skill")
	}
}

func TestLoadSkill_NotFound(t *testing.T) {
	ls := skills.NewLoadSkill(catalogWithSkills())
	res, err := ls.Run(context.Background(), &tools.Env{}, json.RawMessage(`{"name":"nonexistent"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	want := "skill not found: nonexistent"
	if res.Output != want {
		t.Fatalf("unexpected output: %q, want %q", res.Output, want)
	}
}

func TestLoadSkill_AllowedToolsModifier(t *testing.T) {
	ls := skills.NewLoadSkill(catalogWithSkills())
	res, err := ls.Run(context.Background(), &tools.Env{}, json.RawMessage(`{"name":"restricted"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError")
	}
	if res.Modifier == nil {
		t.Fatalf("expected modifier")
	}
	rec := &allowedToolsRecorder{}
	if err := res.Modifier.Apply(rec); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(rec.tools) != 2 || rec.tools[0] != "Read" || rec.tools[1] != "Grep" {
		t.Fatalf("unexpected allowed tools: %v", rec.tools)
	}
}

func TestLoadSkill_Interface(t *testing.T) {
	ls := skills.NewLoadSkill(catalogWithSkills())
	if ls.Name() != "LoadSkill" {
		t.Fatalf("Name() = %q", ls.Name())
	}
	if !ls.IsReadOnly() {
		t.Fatalf("IsReadOnly() = false")
	}
	if !ls.CanRunInParallel() {
		t.Fatalf("CanRunInParallel() = false")
	}
	if ls.Description() == "" {
		t.Fatalf("Description() is empty")
	}
	if len(ls.InputSchema()) == 0 {
		t.Fatalf("InputSchema() is empty")
	}
}

func TestLoadSkill_InvalidJSON(t *testing.T) {
	ls := skills.NewLoadSkill(catalogWithSkills())
	res, err := ls.Run(context.Background(), &tools.Env{}, json.RawMessage(`{invalid`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	if !strings.Contains(res.Output, "invalid input") {
		t.Fatalf("unexpected output: %q", res.Output)
	}
}
