package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestExtractText_ScalarFunction(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single text block",
			input: `[{"type":"text","text":"hello world"}]`,
			want:  "hello world",
		},
		{
			name: "mixed blocks",
			input: `[
				{"type":"text","text":"query"},
				{"type":"tool_use","id":"t1","name":"Read","input":{"path":"a.go"}},
				{"type":"text","text":"result"}
			]`,
			want: "query result",
		},
		{
			name:  "tool_use only yields empty",
			input: `[{"type":"tool_use","id":"t1","name":"Bash","input":{"cmd":"ls"}}]`,
			want:  "",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "malformed json yields empty",
			input: `{not json}`,
			want:  "",
		},
		{
			name:  "empty array",
			input: `[]`,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if err := db.QueryRow("SELECT extract_text(?)", tt.input).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
