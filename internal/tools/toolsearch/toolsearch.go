// Package toolsearch implements the ToolSearch built-in: a provider-agnostic
// tool that lets the model discover deferred tools on demand. Deferred tools
// are withheld from the model's tool list (only their names are announced);
// when the model needs one it calls ToolSearch, which returns the matched
// tools' full JSON schemas in a <functions> block and marks them "discovered"
// so the agent loop injects their real schema into the next request.
//
// This mirrors Claude Code's ToolSearchTool but does NOT depend on the
// Anthropic tool_reference beta — discovery is reconciled entirely client-side
// via the session's discovered set.
package toolsearch

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// Name is the canonical tool name. Re-exported from the tools package so the
// constant has a single source of truth.
const Name = tools.ToolSearchName

const description = `Fetches full schema definitions for deferred tools so they can be called.

Deferred tools appear by name in <system-reminder> messages. Until fetched, only the name is known — there is no parameter schema, so the tool cannot be invoked. This tool takes a query, matches it against the deferred tool list, and returns the matched tools' complete JSONSchema definitions inside a <functions> block. Once a tool's schema appears in that result, it is callable exactly like any tool defined at the top of the prompt.

Result format: each matched tool appears as one <function>{"description": "...", "name": "...", "parameters": {...}}</function> line inside the <functions> block.

Query forms:
- "select:Read,Edit,Grep" — fetch these exact tools by name
- "notebook jupyter" — keyword search, up to max_results best matches
- "+slack send" — require "slack" in the name, rank by remaining terms`

const inputSchema = `{
  "type": "object",
  "properties": {
    "query":       {"type": "string", "description": "Query to find deferred tools. Use \"select:<tool_name>\" for direct selection, or keywords to search."},
    "max_results": {"type": "integer", "description": "Maximum number of results to return (default 5)"}
  },
  "required": ["query"]
}`

// Tool is the ToolSearch built-in. It is stateless; everything it needs comes
// from the per-call Env.ToolCatalog.
type Tool struct{}

func (Tool) Name() string                  { return Name }
func (Tool) Description() string           { return description }
func (Tool) InputSchema() json.RawMessage  { return []byte(inputSchema) }
func (Tool) IsReadOnly() bool              { return true }
func (Tool) CanRunInParallel() bool        { return true }
func (Tool) DefaultTimeout() time.Duration { return 5 * time.Second }

type input struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// discoverModifier marks the matched tools discovered after the batch settles.
type discoverModifier struct{ names []string }

func (m discoverModifier) Apply(t tools.ModifierTarget) error {
	t.MarkToolsDiscovered(m.names)
	return nil
}

// Run resolves the query against the session's deferred tool catalog and
// returns a <functions> block for the matches.
func (Tool) Run(_ context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.Query) == "" {
		return tools.ErrorResultJSON("query is required"), nil
	}
	maxResults := in.MaxResults
	if maxResults <= 0 {
		maxResults = 5
	}

	catalog := deferredFrom(env)

	var matches []tools.ToolInfo
	if rest, ok := cutSelectPrefix(in.Query); ok {
		matches = selectByName(catalog, rest)
	} else {
		matches = searchKeywords(catalog, in.Query, maxResults)
	}

	if len(matches) == 0 {
		return &tools.Result{Output: "No matching deferred tools found."}, nil
	}

	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Name
	}
	return &tools.Result{
		Output:   renderFunctions(matches),
		Modifier: discoverModifier{names: names},
	}, nil
}

// deferredFrom extracts the deferred tool catalog from Env, or nil.
func deferredFrom(env *tools.Env) []tools.ToolInfo {
	if env == nil || env.ToolCatalog == nil {
		return nil
	}
	cat, ok := env.ToolCatalog.(tools.ToolCatalog)
	if !ok {
		return nil
	}
	return cat.DeferredTools()
}

// cutSelectPrefix returns the remainder after a case-insensitive "select:"
// prefix and whether the prefix was present.
func cutSelectPrefix(q string) (string, bool) {
	const p = "select:"
	if len(q) >= len(p) && strings.EqualFold(q[:len(p)], p) {
		return q[len(p):], true
	}
	return "", false
}

// selectByName resolves a comma-separated list of exact tool names against the
// catalog (case-insensitive). Unknown names are skipped; the result preserves
// request order and de-duplicates.
func selectByName(catalog []tools.ToolInfo, list string) []tools.ToolInfo {
	byName := make(map[string]tools.ToolInfo, len(catalog))
	for _, t := range catalog {
		byName[strings.ToLower(t.Name)] = t
	}
	var out []tools.ToolInfo
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		t, ok := byName[strings.ToLower(name)]
		if !ok {
			continue
		}
		if _, dup := seen[t.Name]; dup {
			continue
		}
		seen[t.Name] = struct{}{}
		out = append(out, t)
	}
	return out
}

// parsedName holds a tool name split into searchable parts.
type parsedName struct {
	parts []string
	full  string
}

// parseToolName splits a tool name into lowercase searchable parts, handling
// the server__tool convention (__ and _ separators). Unlike Claude Code there
// is no mcp__ special-case — deferral eligibility is metadata, not a prefix.
func parseToolName(name string) parsedName {
	lower := strings.ToLower(name)
	fields := strings.FieldsFunc(lower, func(r rune) bool { return r == '_' })
	return parsedName{parts: fields, full: strings.Join(fields, " ")}
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// searchKeywords scores each catalog tool against the query terms and returns
// the top maxResults. Scoring mirrors Claude Code's ToolSearchTool:
//   - exact name-part match: +10
//   - name-part substring match: +5
//   - full-name contains term (only if otherwise unscored): +3
//   - description word-boundary match: +2
//
// Terms prefixed with '+' are required: a tool must match all of them (in name
// or description) to be a candidate.
func searchKeywords(catalog []tools.ToolInfo, query string, maxResults int) []tools.ToolInfo {
	q := strings.ToLower(strings.TrimSpace(query))

	// Fast path: query is an exact tool name.
	for _, t := range catalog {
		if strings.ToLower(t.Name) == q {
			return []tools.ToolInfo{t}
		}
	}

	terms := strings.Fields(q)
	if len(terms) == 0 {
		return nil
	}
	var required, optional []string
	for _, term := range terms {
		if len(term) > 1 && strings.HasPrefix(term, "+") {
			required = append(required, term[1:])
		} else {
			optional = append(optional, term)
		}
	}
	scoring := terms
	if len(required) > 0 {
		scoring = append(append([]string{}, required...), optional...)
	}

	type scored struct {
		tool  tools.ToolInfo
		score int
	}
	var results []scored
	for _, t := range catalog {
		pn := parseToolName(t.Name)
		desc := strings.ToLower(t.Description)

		if len(required) > 0 && !matchesAll(required, pn, desc) {
			continue
		}

		score := 0
		for _, term := range scoring {
			switch {
			case containsExact(pn.parts, term):
				score += 10
			case containsSubstr(pn.parts, term):
				score += 5
			}
			if score == 0 && strings.Contains(pn.full, term) {
				score += 3
			}
			if wordBoundary(desc, term) {
				score += 2
			}
		}
		if score > 0 {
			results = append(results, scored{tool: t, score: score})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].tool.Name < results[j].tool.Name
	})
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	out := make([]tools.ToolInfo, len(results))
	for i, r := range results {
		out[i] = r.tool
	}
	return out
}

func matchesAll(required []string, pn parsedName, desc string) bool {
	for _, term := range required {
		if !(containsExact(pn.parts, term) || containsSubstr(pn.parts, term) || wordBoundary(desc, term)) {
			return false
		}
	}
	return true
}

func containsExact(parts []string, term string) bool {
	for _, p := range parts {
		if p == term {
			return true
		}
	}
	return false
}

func containsSubstr(parts []string, term string) bool {
	for _, p := range parts {
		if strings.Contains(p, term) {
			return true
		}
	}
	return false
}

// wordBoundary reports whether term appears as a whole token in text. Tokens
// are maximal runs of [a-z0-9]; term is normalized the same way.
func wordBoundary(text, term string) bool {
	t := nonWord.ReplaceAllString(term, "")
	if t == "" {
		return false
	}
	for _, tok := range nonWord.Split(text, -1) {
		if tok == t {
			return true
		}
	}
	return false
}

// fnEntry is one rendered <function> line. Field order (description, name,
// parameters) matches the top-of-prompt tool encoding.
type fnEntry struct {
	Description string          `json:"description"`
	Name        string          `json:"name"`
	Parameters  json.RawMessage `json:"parameters"`
}

// renderFunctions emits the <functions> block for the matched tools.
func renderFunctions(matches []tools.ToolInfo) string {
	var b strings.Builder
	b.WriteString("<functions>\n")
	for _, m := range matches {
		params := m.InputSchema
		if len(strings.TrimSpace(string(params))) == 0 {
			params = json.RawMessage(`{}`)
		}
		line, err := json.Marshal(fnEntry{Description: m.Description, Name: m.Name, Parameters: params})
		if err != nil {
			continue
		}
		b.WriteString("<function>")
		b.Write(line)
		b.WriteString("</function>\n")
	}
	b.WriteString("</functions>")
	return b.String()
}
