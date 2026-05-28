// Package coord owns the cross-session coordination plumbing for sub-agents:
// the agent_type configuration loader (this file) and the Dispatch tool's
// supporting Host (sibling files in internal/tools/dispatch).
//
// Agent types live in ~/.workhorse-agent/agents/<name>.yaml. The loader
// rescans the directory on every Get/List call so an operator can edit a
// yaml and see the change on the next Dispatch (multi-agent spec §
// "Agent 角色配置").
package coord

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// AgentType is one parsed role definition. Zero-value fields mean "inherit
// from parent or use server default".
type AgentType struct {
	Name          string
	Description   string
	SystemPrompt  string
	AllowTools    []string
	DenyTools     []string
	Provider      string
	Model         string
	MaxIterations int
}

// ErrNotFound is returned by Loader.Get when the requested name doesn't match
// any yaml in the directory.
var ErrNotFound = errors.New("agent_type: not found")

// Loader reads agent_type yamls from a single directory. The directory is
// rescanned on every public call — no cache — so edits are picked up
// immediately.
type Loader struct {
	dir string
	mu  sync.Mutex
}

// NewLoader takes the directory path (e.g. ~/.workhorse-agent/agents).
func NewLoader(dir string) *Loader {
	return &Loader{dir: dir}
}

// Dir returns the configured directory.
func (l *Loader) Dir() string { return l.dir }

// Get returns one AgentType by name. Returns wrapped ErrNotFound when the
// name isn't present.
func (l *Loader) Get(name string) (AgentType, error) {
	all, err := l.List()
	if err != nil {
		return AgentType{}, err
	}
	for _, a := range all {
		if a.Name == name {
			return a, nil
		}
	}
	return AgentType{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// List rescans the directory, merges embedded builtins, and returns every
// AgentType in deterministic (alphabetical) order. A missing directory is
// fine — the builtins still appear. On-disk entries with the same name
// override the builtin, EXCEPT that adapter-generator's effective tool
// surface is always re-clamped by applyAdapterGeneratorLockdown so a
// hand-edited yaml cannot escalate privileges.
func (l *Loader) List() ([]AgentType, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	merged := map[string]AgentType{}
	for _, at := range loadBuiltinAgentsOrEmpty() {
		merged[at.Name] = at
	}

	if l.dir != "" {
		entries, err := os.ReadDir(l.dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// No on-disk overrides; builtins-only is a valid configuration.
		case err != nil:
			return nil, fmt.Errorf("agent_type: read dir %q: %w", l.dir, err)
		default:
			seen := map[string]string{}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				base := e.Name()
				if !strings.HasSuffix(base, ".yaml") && !strings.HasSuffix(base, ".yml") {
					continue
				}
				path := filepath.Join(l.dir, base)
				at, err := parseFile(path)
				if err != nil {
					return nil, err
				}
				if prev, dup := seen[at.Name]; dup {
					return nil, fmt.Errorf("agent_type: duplicate name %q (in %q and %q)",
						at.Name, prev, path)
				}
				seen[at.Name] = path
				applyAdapterGeneratorLockdown(&at)
				merged[at.Name] = at
			}
		}
	}

	out := make([]AgentType, 0, len(merged))
	for _, at := range merged {
		out = append(out, at)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// loadBuiltinAgentsOrEmpty wraps loadBuiltinAgents, swallowing errors. A
// failure to load embedded agents is a build-time bug, not a user-visible
// runtime error; the rest of the agent type system should keep working with
// whatever did parse (or nothing).
func loadBuiltinAgentsOrEmpty() []AgentType {
	out, err := loadBuiltinAgents()
	if err != nil {
		return nil
	}
	return out
}

type agentYAML struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	SystemPrompt string `yaml:"system_prompt"`
	Tools        struct {
		Allow []string `yaml:"allow"`
		Deny  []string `yaml:"deny"`
	} `yaml:"tools"`
	Provider      string `yaml:"provider"`
	Model         string `yaml:"model"`
	MaxIterations int    `yaml:"max_iterations"`
}

func parseFile(path string) (AgentType, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentType{}, fmt.Errorf("agent_type: read %q: %w", path, err)
	}
	var y agentYAML
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return AgentType{}, fmt.Errorf("agent_type: parse %q: %w", path, err)
	}
	if y.Name == "" {
		stem := filepath.Base(path)
		stem = strings.TrimSuffix(stem, filepath.Ext(stem))
		y.Name = stem
	}
	if y.MaxIterations <= 0 {
		y.MaxIterations = 50
	}
	return AgentType{
		Name:          y.Name,
		Description:   y.Description,
		SystemPrompt:  y.SystemPrompt,
		AllowTools:    append([]string(nil), y.Tools.Allow...),
		DenyTools:     append([]string(nil), y.Tools.Deny...),
		Provider:      y.Provider,
		Model:         y.Model,
		MaxIterations: y.MaxIterations,
	}, nil
}
