package idgen

import (
	"regexp"
	"sort"
	"testing"
	"time"
)

// canonical Crockford alphabet, no I/L/O/U
var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestNewULID_Format(t *testing.T) {
	id := NewULID()
	if !ulidRe.MatchString(id) {
		t.Fatalf("ULID %q does not match Crockford base32 26-char pattern", id)
	}
}

func TestNewULID_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewULID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ULID at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewULID_TimeOrdered(t *testing.T) {
	// IDs minted across distinct milliseconds must sort lexically by time.
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, NewULID())
		time.Sleep(2 * time.Millisecond)
	}
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ULIDs not time-ordered: %v vs sorted %v", ids, sorted)
		}
	}
}
