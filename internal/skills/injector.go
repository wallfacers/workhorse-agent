package skills

import (
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

func FormatManifest(cat *Catalog) string {
	if cat == nil || len(cat.Skills) == 0 {
		return ""
	}
	items := make([]map[string]string, len(cat.Skills))
	for i, s := range cat.Skills {
		items[i] = map[string]string{
			"Name":    s.Name,
			"Trigger": strings.Join(strings.Fields(s.Trigger), " "),
		}
	}
	out, err := prompt.SkillManifest.Execute(map[string]any{"Skills": items})
	if err != nil {
		return ""
	}
	return out
}
