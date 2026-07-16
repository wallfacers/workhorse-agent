// Package pipeline implements the ADD-only fact extraction path
// (memory-hybrid-retrieval-locomo). It distills a batch of conversation
// messages into new memory entries using exactly one LLM call per batch: every
// extracted fact becomes a NEW entry (it never updates or deletes an existing
// one), entities are indexed for the entity retrieval signal, and event dates
// are stamped for time-aware retrieval. Redundancy from accumulation is left to
// the existing curation engine.
//
// The pipeline is fail-safe: a model/parse error, or an individual invalid fact,
// is a WARN and a skip — never a session-affecting error.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/idgen"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

// ModelCaller performs one text-in/text-out model call (same shape as the
// curation judge caller). The runtime wires a real provider; tests inject a mock.
type ModelCaller func(ctx context.Context, system, user string) (string, error)

// Message is one conversation turn fed to the pipeline.
type Message struct {
	Role string // "user" or "assistant"
	Text string
}

// Pipeline extracts and stores facts. A nil call makes it inert (Ingest is a
// no-op), mirroring the curation worker's inert mode.
type Pipeline struct {
	entries  *memory.EntryStore
	embedder *memory.Embedder // may be nil (embedding disabled)
	call     ModelCaller
	budgets  memory.Budgets
	onWrite  func() // curation pressure trigger; optional
}

// Config bundles the pipeline's dependencies.
type Config struct {
	Entries  *memory.EntryStore
	Embedder *memory.Embedder
	Call     ModelCaller
	Budgets  memory.Budgets
	OnWrite  func()
}

// New builds a Pipeline. Returns nil when Entries or Call is nil (inert).
func New(cfg Config) *Pipeline {
	if cfg.Entries == nil || cfg.Call == nil {
		return nil
	}
	return &Pipeline{
		entries:  cfg.Entries,
		embedder: cfg.Embedder,
		call:     cfg.Call,
		budgets:  cfg.Budgets,
		onWrite:  cfg.OnWrite,
	}
}

type extractedFact struct {
	Fact       string   `json:"fact"`
	Entities   []string `json:"entities"`
	EventDate  string   `json:"event_date"`
	Category   string   `json:"category"`
	Durability string   `json:"durability"`
}

type extractionResult struct {
	Facts []extractedFact `json:"facts"`
}

// Ingest runs one extraction pass over messages dated sessionDate. It returns the
// number of entries written. A nil pipeline, a trivial batch, or any failure
// yields (0, nil) — the caller never needs to handle extraction errors.
func (p *Pipeline) Ingest(ctx context.Context, sessionDate time.Time, sourceSessionID string, messages []Message) (int, error) {
	if p == nil {
		return 0, nil
	}
	if !hasSubstance(messages) {
		return 0, nil // trivial batch: no LLM call
	}

	promptMsgs := make([]prompt.MemoryExtractionMessage, 0, len(messages))
	for _, m := range messages {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		promptMsgs = append(promptMsgs, prompt.MemoryExtractionMessage{Role: m.Role, Text: m.Text})
	}
	dateStr := ""
	if !sessionDate.IsZero() {
		dateStr = sessionDate.UTC().Format("2006-01-02")
	}
	user := prompt.BuildMemoryExtractionUserPrompt(dateStr, promptMsgs)

	raw, err := p.call(ctx, prompt.MemoryExtractionSystemPrompt, user)
	if err != nil {
		slog.Warn("memory: extraction model call failed", "err", err)
		return 0, nil
	}
	facts, err := parseFacts(raw)
	if err != nil {
		slog.Warn("memory: extraction parse failed", "err", err)
		return 0, nil
	}

	written := 0
	for _, f := range facts.Facts {
		if p.storeFact(ctx, sessionDate, sourceSessionID, f) {
			written++
		}
	}
	if written > 0 && p.onWrite != nil {
		p.onWrite() // one curation pressure signal per batch
	}
	return written, nil
}

// storeFact validates and persists one extracted fact. Returns true on success.
func (p *Pipeline) storeFact(ctx context.Context, sessionDate time.Time, sourceSessionID string, f extractedFact) bool {
	content := strings.TrimSpace(f.Fact)
	if content == "" {
		return false
	}
	if err := p.budgets.CheckEntryContent(content); err != nil {
		slog.Warn("memory: extracted fact rejected", "reason", "content_too_large", "err", err)
		return false
	}
	trigger := deriveTrigger(content, p.budgets.TriggerChars)
	if err := p.budgets.CheckTrigger(trigger); err != nil {
		trigger = "" // a bad derived trigger is non-fatal; store without one
	}

	entry := &memory.Entry{
		Name:            entryName(content),
		Trigger:         trigger,
		Content:         content,
		Durability:      normalizeDurability(f.Durability),
		Category:        strings.TrimSpace(f.Category),
		CharCount:       memory.CharCount(content),
		SourceSessionID: sourceSessionID,
		FactSource:      "extraction",
		EventDate:       parseEventDate(f.EventDate, sessionDate),
	}
	if err := p.entries.Upsert(ctx, entry); err != nil {
		slog.Warn("memory: extracted fact upsert failed", "name", entry.Name, "err", err)
		return false
	}
	if len(f.Entities) > 0 {
		if err := p.entries.PutEntities(ctx, entry.Name, f.Entities); err != nil {
			slog.Warn("memory: extracted entities index failed", "name", entry.Name, "err", err)
		}
	}
	p.embedder.Enqueue(entry.Name) // nil-safe
	return true
}

// hasSubstance reports whether the batch has any non-empty user/assistant text.
func hasSubstance(messages []Message) bool {
	for _, m := range messages {
		if strings.TrimSpace(m.Text) != "" {
			return true
		}
	}
	return false
}

func parseFacts(raw string) (*extractionResult, error) {
	js := extractJSON(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object in extraction output")
	}
	var r extractionResult
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		return nil, fmt.Errorf("unmarshal extraction JSON: %w", err)
	}
	return &r, nil
}

func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

func normalizeDurability(d string) string {
	switch strings.TrimSpace(strings.ToLower(d)) {
	case "evergreen":
		return "evergreen"
	default:
		return "volatile"
	}
}

// parseEventDate resolves an ISO date string; on failure returns nil. sessionDate
// is reserved for future relative-date resolution (the model already resolves
// relatives against the session date it is given).
func parseEventDate(s string, _ time.Time) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// deriveTrigger produces a short recall cue from the fact by truncating to the
// trigger budget on a rune boundary.
func deriveTrigger(fact string, maxRunes int) string {
	fact = oneLine(fact)
	if maxRunes <= 0 {
		maxRunes = 120
	}
	r := []rune(fact)
	if len(r) <= maxRunes {
		return fact
	}
	return strings.TrimSpace(string(r[:maxRunes-1]))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// entryName builds a unique slug: a truncated kebab-case slug of the fact plus a
// ULID suffix, so ADD-only never collides on an existing name.
func entryName(fact string) string {
	slug := slugify(fact, 40)
	suffix := strings.ToLower(idgen.NewULID())
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	if slug == "" {
		return "fact-" + suffix
	}
	return slug + "-" + suffix
}

// slugify lowercases, keeps ASCII alphanumerics and CJK runes, and joins runs
// with single hyphens, capped at maxRunes.
func slugify(s string, maxRunes int) string {
	var b strings.Builder
	prevHyphen := false
	count := 0
	for _, r := range strings.ToLower(s) {
		if count >= maxRunes {
			break
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || isCJKRune(r):
			b.WriteRune(r)
			prevHyphen = false
			count++
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
				count++
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func isCJKRune(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3040 && r <= 0x30FF) || // Hiragana + Katakana
		(r >= 0xAC00 && r <= 0xD7A3) // Hangul syllables
}
