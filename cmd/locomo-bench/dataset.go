package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LoCoMo dataset structures. The public benchmark ships a JSON array; each item
// has a `conversation` object (speaker names, per-session date-time strings, and
// per-session turn arrays keyed session_1, session_2, …) plus a `qa` array.

type locomoItem struct {
	QA           []locomoQA      `json:"qa"`
	Conversation json.RawMessage `json:"conversation"`
}

type locomoQA struct {
	Question string          `json:"question"`
	Answer   json.RawMessage `json:"answer"` // may be string or number
	Category int             `json:"category"`
}

// AnswerText renders the gold answer as a string regardless of JSON type.
func (q locomoQA) AnswerText() string {
	if len(q.Answer) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(q.Answer, &s); err == nil {
		return s
	}
	return strings.Trim(string(q.Answer), `"`)
}

// session is one dated block of dialogue turns.
type session struct {
	Index int
	Date  time.Time
	Turns []turn
}

type turn struct {
	Speaker string
	Text    string
}

// conversation is the parsed, ordered set of sessions plus the QA list.
type conversation struct {
	ID       int
	Sessions []session
	QA       []locomoQA
}

// loadDataset reads and parses the LoCoMo JSON file.
func loadDataset(path string) ([]conversation, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied benchmark path
	if err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	var items []locomoItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse dataset JSON (expected a top-level array): %w", err)
	}
	convs := make([]conversation, 0, len(items))
	for i, it := range items {
		sessions, err := parseConversation(it.Conversation)
		if err != nil {
			return nil, fmt.Errorf("conversation %d: %w", i, err)
		}
		convs = append(convs, conversation{ID: i, Sessions: sessions, QA: it.QA})
	}
	return convs, nil
}

// parseConversation extracts the session_N / session_N_date_time fields from the
// dynamic conversation object.
func parseConversation(raw json.RawMessage) ([]session, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("conversation object: %w", err)
	}
	byIndex := map[int]*session{}
	for key, val := range obj {
		if !strings.HasPrefix(key, "session_") {
			continue
		}
		rest := strings.TrimPrefix(key, "session_")
		if strings.Contains(rest, "date_time") {
			idxStr := strings.TrimSuffix(rest, "_date_time")
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				continue
			}
			var dt string
			_ = json.Unmarshal(val, &dt)
			s := ensureSession(byIndex, idx)
			s.Date = parseLoCoMoDate(dt)
			continue
		}
		idx, err := strconv.Atoi(rest)
		if err != nil {
			continue // session_1_date_time already handled; skip other shapes
		}
		var turns []struct {
			Speaker string `json:"speaker"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(val, &turns); err != nil {
			continue
		}
		s := ensureSession(byIndex, idx)
		for _, tt := range turns {
			if strings.TrimSpace(tt.Text) == "" {
				continue
			}
			s.Turns = append(s.Turns, turn{Speaker: tt.Speaker, Text: tt.Text})
		}
	}

	out := make([]session, 0, len(byIndex))
	for _, s := range byIndex {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

func ensureSession(m map[int]*session, idx int) *session {
	if s, ok := m[idx]; ok {
		return s
	}
	s := &session{Index: idx}
	m[idx] = s
	return s
}

// parseLoCoMoDate parses the dataset's human date strings, e.g.
// "1:56 pm on 8 May, 2023" or "7:00 pm on 25 May, 2023". Returns the zero time
// when unparseable (the pipeline tolerates a zero session date).
func parseLoCoMoDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	// Strip the leading time-of-day "... on " prefix if present.
	if idx := strings.Index(s, " on "); idx >= 0 {
		s = s[idx+4:]
	}
	for _, layout := range []string{"2 January, 2006", "2 Jan, 2006", "January 2, 2006", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// categoryLabel maps LoCoMo integer categories to names. Category 5 is the
// adversarial set, excluded from scoring per the Mem0 convention.
func categoryLabel(c int) string {
	switch c {
	case 1:
		return "multi-hop"
	case 2:
		return "temporal"
	case 3:
		return "open-domain"
	case 4:
		return "single-hop"
	case 5:
		return "adversarial"
	default:
		return "category-" + strconv.Itoa(c)
	}
}

const adversarialCategory = 5
