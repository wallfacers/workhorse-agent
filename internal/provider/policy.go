package provider

import "strings"

// ModelPolicy resolves the (provider, model) tuple for any given call. The
// rules come from the provider-abstraction spec:
//
//  1. An explicit override (e.g. "anthropic:claude-opus-4-7") wins.
//  2. Otherwise BySessionType is consulted by agent type.
//  3. Otherwise Default is used.
//
// PickFast applies the "same family" rule for the auto-compaction call:
// compress with Haiku for Anthropic sessions and gpt-4o-mini for OpenAI
// sessions, never crossing families.
type ModelPolicy struct {
	Default       string
	Fast          string
	BySessionType map[string]string
}

// Pick returns the provider name and model id for the given context. agentType
// may be empty (root session). override may be empty (no per-call override).
func (p ModelPolicy) Pick(agentType, override string) (string, string) {
	if override != "" {
		return SplitProviderModel(override)
	}
	if agentType != "" {
		if v, ok := p.BySessionType[agentType]; ok && v != "" {
			return SplitProviderModel(v)
		}
	}
	return SplitProviderModel(p.Default)
}

// PickFast applies the same-family rule. sessionProvider is the provider name
// the session is currently using (e.g. "anthropic"). When Fast points to a
// different family we fall back to the known fast model for the session's
// family so a compaction never crosses vendors.
func (p ModelPolicy) PickFast(sessionProvider string) (string, string) {
	fastProvider, fastModel := SplitProviderModel(p.Fast)
	if sessionProvider == "" || fastProvider == sessionProvider {
		return fastProvider, fastModel
	}
	switch sessionProvider {
	case "anthropic":
		return "anthropic", "claude-haiku-4-5-20251001"
	case "openai":
		return "openai", "gpt-4o-mini"
	default:
		// Unknown family — best effort: return the configured fast model.
		return fastProvider, fastModel
	}
}

// SplitProviderModel parses "<provider>:<model-id>". If no colon is present
// the whole string is returned as the model id with an empty provider; the
// caller has to decide what to do (typically: error).
func SplitProviderModel(s string) (string, string) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", s
	}
	return s[:i], s[i+1:]
}
