// Package sqlite registers custom SQLite functions needed by workhorse-agent.
//
// Registration API (confirmed via spike, modernc.org/sqlite v1.34.5):
//
//	sqlite.MustRegisterScalarFunction(name, nArg, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error))
//
// Functions registered this way are available to all new connections opened
// after the call. MustRegister* panics on error — call during package init
// or program startup.
package sqlite

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"modernc.org/sqlite"
)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("extract_text", 1, extractTextFn)
}

// extractTextFn is the implementation of the extract_text SQLite function.
// It takes a content_json BLOB/TEXT argument, parses it as a JSON array of
// content blocks, and concatenates the "text" field of blocks where
// type == "text", joined by single spaces.
func extractTextFn(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 {
		return "", nil
	}

	raw, ok := args[0].(string)
	if !ok {
		b, ok := args[0].([]byte)
		if !ok {
			return "", nil
		}
		raw = string(b)
	}
	if raw == "" {
		return "", nil
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		slog.Warn("sqlite extract_text: malformed content_json", "err", err)
		return "", nil
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " "), nil
}

// ProbeFTS5 checks whether the linked SQLite build has FTS5 compiled in.
func ProbeFTS5(db *sql.DB) error {
	var enabled int
	if err := db.QueryRow("SELECT sqlite_compileoption_used('ENABLE_FTS5')").Scan(&enabled); err != nil {
		return fmt.Errorf("sqlite: FTS5 probe: %w", err)
	}
	if enabled == 0 {
		return fmt.Errorf("sqlite: FTS5 is not compiled into the linked modernc.org/sqlite build; " +
			"this is required for session search. Please report this issue at " +
			"https://github.com/wallfacers/workhorse-agent/issues")
	}
	return nil
}
