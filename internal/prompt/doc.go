// Package prompt centrally manages all LLM-facing prompts used by the agent
// runtime. It provides compiled text/template instances, constant fallback
// strings, and convenience wrappers.
//
// # What belongs here
//
// Any text that is sent to a language model as part of a prompt (system
// prompts, tool-injection paragraphs, summariser instructions) lives in this
// package. This does NOT include:
//   - Tool Description/InputSchema strings — they are code-coupled metadata.
//   - Output markers like "[truncated]" — they are format constants, not prompts.
//   - CLI or HTTP error messages — they target humans, not LLMs.
//
// # Language standard
//
// All prompts SHALL be written in English for maximum LLM instruction-following
// accuracy and consistency.
//
// # Placeholder convention
//
// Template placeholders use PascalCase (e.g. {{.BasePrompt}}, {{.Name}}).
//
// # Template engine
//
// Uses text/template (not html/template — output is never HTML).
//
// # Security constraints
//
// Execute accepts map[string]any where values MUST be basic types (string,
// int, bool) or []map[string]string. Passing structs, funcs, or chans is
// forbidden to prevent template method-call leaks. text/template treats data
// values as raw strings, providing native SSTI immunity.
//
// # Forbidden imports
//
// To prevent circular dependencies, this package MUST NOT import any of:
// internal/agent, internal/skills, internal/tools, internal/config,
// internal/session, internal/coord, internal/provider, internal/api,
// internal/store.
package prompt
