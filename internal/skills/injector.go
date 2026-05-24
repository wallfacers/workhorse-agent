package skills

import (
	"fmt"
	"strings"
)

func FormatManifest(cat *Catalog) string {
	if cat == nil || len(cat.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, s := range cat.Skills {
		trigger := strings.Join(strings.Fields(s.Trigger), " ")
		fmt.Fprintf(&b, "- name: %s\n  trigger: %s\n", s.Name, trigger)
	}
	b.WriteString("</available_skills>\n")
	b.WriteString("\n可以调用 LoadSkill 工具加载完整指令。\n")
	return b.String()
}
