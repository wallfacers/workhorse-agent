package prompt

import (
	"fmt"
	"strings"
)

// MemoryExtractionMessage is one conversation turn presented to the extraction
// model. Plain data so the prompt package stays IO-free and memory-independent.
type MemoryExtractionMessage struct {
	Role string // "user" or "assistant"
	Text string
}

// MemoryExtractionSystemPrompt instructs a small model to distill durable facts
// from a conversation batch into strict JSON (memory-hybrid-retrieval-locomo,
// ADD-only pipeline). It mirrors the Mem0 v3 extraction contract: user AND
// agent-confirmed facts are first-class, entities are surfaced for linking, and
// event dates are resolved against the session date. Conservative: no facts →
// empty array, never invented content.
const MemoryExtractionSystemPrompt = `You extract self-contained memories from a conversation for an AI agent's long-term memory. You are given the session date and a batch of messages. Be COMPREHENSIVE: capture every concrete, recallable fact and event a future question could ask about — do not summarize or drop specifics. Return STRICT JSON only.

What to extract (one memory per distinct fact):
- Concrete events and actions with their details: who did what, when, where, with whom, and the outcome (e.g. "Jon lost his job as a banker on 2023-01-19", "Gina teamed up with a local artist in February 2023"). Capture EACH event separately, including specific dates, quantities, names, places, and results — these are exactly what questions probe.
- Stable facts about a person: identity, preferences, relationships, possessions, occupation, plans (e.g. "Caroline is a transgender woman").
- Facts the assistant confirmed or committed to (agent actions, decisions) — weight them equally to user statements.
- Each "fact" MUST be a single self-contained sentence understandable with no surrounding context: resolve pronouns and name the subject explicitly.
- "entities": the salient named entities in the fact (people, places, organizations, products, concepts).
- "event_date": if the fact happened at a time, resolve it to an ISO date (YYYY-MM-DD, or YYYY-MM / YYYY if only month/year is known). Resolve relative expressions ("last month", "four years ago", "two weeks ago") against the SESSION DATE, never against today. Omit only when there is genuinely no time reference.
- "category": one of user, agent, preference, event, reference. "durability": "evergreen" for stable traits, "volatile" for datable events and changeable states.

What NOT to extract:
- Pure greetings, filler, and questions that assert no fact.
- Never invent facts, entities, or dates. Omit an uncertain field rather than guessing — but do not drop a real fact just because one field is unknown.

Keep it tight: each "fact" is ONE short sentence (no compound clauses — split them). Merge near-duplicates. Cover every distinct event and trait, but do not pad; most sessions yield a handful to ~20 facts.

Output shape (STRICT JSON, no markdown, no prose):
{"facts":[{"fact":"...","entities":["..."],"event_date":"YYYY-MM-DD","category":"event","durability":"volatile"}]}
If there is genuinely nothing to remember, output {"facts":[]}.`

// BuildMemoryExtractionUserPrompt renders the session date and message batch into
// the user message for one extraction pass.
func BuildMemoryExtractionUserPrompt(sessionDate string, messages []MemoryExtractionMessage) string {
	var b strings.Builder
	if sessionDate != "" {
		fmt.Fprintf(&b, "SESSION DATE: %s\n\n", sessionDate)
	}
	b.WriteString("CONVERSATION:\n")
	for _, m := range messages {
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		fmt.Fprintf(&b, "%s: %s\n", role, oneLine(m.Text))
	}
	b.WriteString("\nReturn the JSON now.")
	return b.String()
}
