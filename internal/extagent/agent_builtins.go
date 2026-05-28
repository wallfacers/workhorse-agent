package extagent

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"
)

// Embedded agent-type YAML descriptors shipped with the binary. These live
// alongside the adapter builtins (claude-code.yaml, codex.yaml, aider.yaml)
// in the builtins/ tree but in a separate subdirectory so the adapter loader
// can keep its non-recursive glob. Consumers fetch them through
// BuiltinAgentYAML(name) — see internal/coord which assembles AgentType
// values out of them.
//
//go:embed builtins/agents/*.yaml
var builtinAgentsFS embed.FS

// BuiltinAgentYAML returns the raw YAML bytes for the embedded agent-type
// with the given name (no extension). Returns ("", false) when no builtin
// matches. The name → filename mapping is identity: "adapter-generator" →
// builtins/agents/adapter-generator.yaml.
func BuiltinAgentYAML(name string) ([]byte, bool) {
	if name == "" {
		return nil, false
	}
	path := filepath.Join("builtins/agents", name+".yaml")
	raw, err := builtinAgentsFS.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// BuiltinAdapterYAML returns the raw YAML bytes of an embedded adapter
// (claude-code, codex, aider, …). Returns ("", false) when no builtin
// matches. Used by agent_setup to feed few-shot examples into the
// AdapterGeneration prompt template.
func BuiltinAdapterYAML(name string) ([]byte, bool) {
	if name == "" {
		return nil, false
	}
	raw, err := builtinFS.ReadFile(filepath.Join("builtins", name+".yaml"))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// AdapterSchemaJSON returns the embedded adapter JSON schema bytes. The
// boolean is false only when the embed itself was somehow lost — fail loud
// upstream rather than silently emit "{}".
func AdapterSchemaJSON() ([]byte, bool) {
	if len(adapterSchemaJSON) == 0 {
		return nil, false
	}
	return adapterSchemaJSON, true
}

// BuiltinAgentNames returns the names of every embedded agent-type yaml in
// alphabetical order. Callers use this to enumerate available builtins
// (e.g. for tests and for the coord layer's fallback loader).
func BuiltinAgentNames() ([]string, error) {
	entries, err := builtinAgentsFS.ReadDir("builtins/agents")
	if err != nil {
		return nil, fmt.Errorf("extagent: read embedded agents: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		out = append(out, strings.TrimSuffix(ent.Name(), ".yaml"))
	}
	return out, nil
}
