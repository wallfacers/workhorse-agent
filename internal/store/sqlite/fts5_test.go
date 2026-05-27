package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestFTS5Available(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var enabled int
	if err := db.QueryRow("SELECT sqlite_compileoption_used('ENABLE_FTS5')").Scan(&enabled); err != nil {
		t.Fatalf("query: %v", err)
	}
	if enabled == 0 {
		t.Fatal("FTS5 is not compiled into the linked modernc.org/sqlite build")
	}
}
