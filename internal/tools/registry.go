package tools

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
)

// Registry holds every Tool the agent can dispatch — builtins, MCP-adapted
// tools, Dispatch, and LoadSkill. The set is built at startup; AllowedTools
// is a per-session filter applied on lookup.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds t. Names must be unique; double-registration is rejected so a
// typo in MCP namespacing doesn't silently shadow a builtin.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("registry: nil tool")
	}
	name := t.Name()
	if name == "" {
		return errors.New("registry: tool has empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("registry: tool %q already registered", name)
	}
	r.tools[name] = t
	return nil
}

// Unregister removes the tool with the given name; missing names are ignored.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Clone returns a shallow copy of the registry. The returned Registry shares
// the same Tool instances (which are stateless or thread-safe by contract)
// but holds its own map, so callers can Register or Replace entries without
// touching the source. Used by the runner factory to assemble per-session
// tool surfaces (e.g. adapter-generator overlays).
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := NewRegistry()
	for n, t := range r.tools {
		out.tools[n] = t
	}
	return out
}

// Replace registers t under its Name(), overwriting any prior entry. Used
// when a per-session overlay wants to substitute a stricter version of an
// existing tool (e.g. swapping Bash for the adapter-generator's restricted
// genbash variant).
func (r *Registry) Replace(t Tool) error {
	if t == nil {
		return errors.New("registry: nil tool")
	}
	name := t.Name()
	if name == "" {
		return errors.New("registry: tool has empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[name] = t
	return nil
}

// Get returns the named tool. The second result is false when no such tool
// exists.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the registered tool names in deterministic order so system
// prompts are stable between calls.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Filtered returns the subset of registered tools whose name matches an
// allowed entry. Entries containing glob metacharacters (* ? [) match by
// path.Match — tool names contain no `/`, so this equals the permission
// rules' single-segment glob; "dataweave__*" admits every tool of that MCP
// server, including ones registered later. Metachar-free entries keep the
// original exact-match semantics. A nil or empty allowed slice means "no
// filter" — every tool is returned.
func (r *Registry) Filtered(allowed []string) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(allowed) == 0 {
		out := make([]Tool, 0, len(r.tools))
		for _, n := range sortedKeys(r.tools) {
			out = append(out, r.tools[n])
		}
		return out
	}
	exact := make(map[string]struct{}, len(allowed))
	var globs []string
	for _, a := range allowed {
		if strings.ContainsAny(a, "*?[") {
			globs = append(globs, a)
		} else {
			exact[a] = struct{}{}
		}
	}
	out := make([]Tool, 0, len(allowed))
	for _, n := range sortedKeys(r.tools) {
		if _, ok := exact[n]; ok {
			out = append(out, r.tools[n])
			continue
		}
		for _, g := range globs {
			if ok, err := path.Match(g, n); err == nil && ok {
				out = append(out, r.tools[n])
				break
			}
		}
	}
	return out
}

func sortedKeys(m map[string]Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
