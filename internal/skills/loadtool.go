package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

const loadSkillSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Name of the skill to load"}
  },
  "required": ["name"]
}`

type LoadSkillInput struct {
	Name string `json:"name"`
}

type LoadSkill struct {
	catalog *Catalog
	timeout time.Duration
}

func NewLoadSkill(catalog *Catalog) *LoadSkill {
	return &LoadSkill{catalog: catalog}
}

func (LoadSkill) Name() string             { return "LoadSkill" }
func (LoadSkill) IsReadOnly() bool         { return true }
func (LoadSkill) CanRunInParallel() bool   { return true }
func (LoadSkill) Description() string {
	return "Load a skill's full instructions by name. Returns the skill content for the LLM to use."
}
func (LoadSkill) InputSchema() json.RawMessage { return []byte(loadSkillSchema) }

func (l *LoadSkill) DefaultTimeout() time.Duration {
	if l.timeout > 0 {
		return l.timeout
	}
	return 30 * time.Second
}

func (l *LoadSkill) Run(_ context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in LoadSkillInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return &tools.Result{Output: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if in.Name == "" {
		return &tools.Result{Output: "name is required", IsError: true}, nil
	}
	skill := l.catalog.Get(in.Name)
	if skill == nil {
		return &tools.Result{
			Output:  fmt.Sprintf("skill not found: %s", in.Name),
			IsError: true,
		}, nil
	}
	res := &tools.Result{Output: skill.Content}
	if len(skill.AllowedTools) > 0 {
		res.Modifier = AllowedToolsModifier{Tools: skill.AllowedTools}
	}
	return res, nil
}

type AllowedToolsModifier struct {
	Tools []string
}

func (m AllowedToolsModifier) Apply(target tools.ModifierTarget) error {
	target.SetAllowedTools(m.Tools)
	return nil
}
