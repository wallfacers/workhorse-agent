package main

import (
	"testing"
	"unicode"

	"github.com/wallfacers/workhorse-agent/internal/skills"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/agentsetup"
	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
	"github.com/wallfacers/workhorse-agent/internal/tools/builtin"
	delegationtool "github.com/wallfacers/workhorse-agent/internal/tools/delegation"
	"github.com/wallfacers/workhorse-agent/internal/tools/dispatch"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/drafttool"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/genbash"
	"github.com/wallfacers/workhorse-agent/internal/tools/memorytool"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
	"github.com/wallfacers/workhorse-agent/internal/tools/tasklist"
	"github.com/wallfacers/workhorse-agent/internal/tools/toolsearch"
)

// localStaticTools enumerates the built-in tools whose Description() is a
// static string we own (excludes ExternalAgent and frontend tools, whose
// descriptions are derived from external adapters / client catalogs). Their
// descriptions feed the always-loaded tool list and MUST be English: Latin
// letters only. Typographic punctuation (em dash, curly quotes) is allowed;
// non-Latin letters (CJK, Cyrillic, …) are not.
func localStaticTools() []tools.Tool {
	return []tools.Tool{
		builtin.Read{},
		builtin.Write{},
		builtin.Edit{},
		builtin.Grep{},
		bash.Bash{},
		&memorytool.Read{},
		&memorytool.Write{},
		memorytool.NewLoadMemory(nil, nil),
		&memorytool.MemorySearch{},
		&memorytool.Delete{},
		&memorytool.Merge{},
		&sessionsearch.Tool{},
		tasklist.TodoWrite{},
		toolsearch.Tool{},
		delegationtool.DelegateTool{},
		delegationtool.DelegationReadTool{},
		delegationtool.DelegationListTool{},
		dispatch.Tool{},
		agentsetup.Tool{},
		genbash.Tool{},
		drafttool.Tool{},
		skills.NewLoadSkill(nil),
	}
}

func TestLocalToolDescriptionsAreEnglish(t *testing.T) {
	for _, tool := range localStaticTools() {
		desc := tool.Description()
		if desc == "" {
			t.Errorf("tool %q has empty description", tool.Name())
			continue
		}
		for i, r := range desc {
			if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
				t.Errorf("tool %q description has non-Latin letter %q at byte %d; local tool descriptions must be English", tool.Name(), r, i)
				break
			}
		}
	}
}
