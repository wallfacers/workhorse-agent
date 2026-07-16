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
const MemoryExtractionSystemPrompt = `You extract durable, self-contained memories from a conversation for an AI agent's long-term memory. You are given the session date and a batch of messages. Return STRICT JSON only.

What to extract:
- Stable facts about the user (preferences, identity, relationships, possessions, plans) and facts the assistant confirmed or committed to (agent actions, decisions). Treat assistant-confirmed facts with equal weight to user statements.
- One memory per distinct fact. Each "fact" MUST be a single self-contained sentence understandable without the surrounding conversation (resolve pronouns; name the subject).
- For each fact, list the salient named entities it mentions (people, places, organizations, products, concepts) in "entities".
- If the fact refers to a time when something happened, resolve it to an ISO date "event_date" (YYYY-MM-DD; use the session date to resolve relative expressions like "four years ago" or "last Tuesday"; omit if there is no clear event time).
- Set "category" to one of: user, agent, preference, event, reference. Set "durability" to "evergreen" for stable facts or "volatile" for things that may change soon.

What NOT to extract:
- Small talk, questions, transient task chatter, or anything not worth recalling in a future session.
- Never invent facts, entities, or dates. Omit uncertain fields.

Output shape (STRICT JSON, no markdown, no prose):
{"facts":[{"fact":"...","entities":["..."],"event_date":"YYYY-MM-DD","category":"user","durability":"evergreen"}]}
If there is nothing worth remembering, output {"facts":[]}.`

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
