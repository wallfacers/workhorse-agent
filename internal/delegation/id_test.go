package delegation

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerate_Format(t *testing.T) {
	cases := []struct {
		name string
		list []string
		idx  int
	}{
		{"adjective slot", adjectives, 0},
		{"color slot", colors, 1},
		{"animal slot", animals, 2},
	}
	for i := 0; i < 300; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		parts := strings.Split(id, "-")
		if len(parts) != 3 {
			t.Fatalf("id %q: want 3 dash-separated parts", id)
		}
		for _, c := range cases {
			if !contains(c.list, parts[c.idx]) {
				t.Fatalf("id %q: %s %q not in its word list", id, c.name, parts[c.idx])
			}
		}
	}
}

func TestGenerateUnique_Accepted(t *testing.T) {
	id, err := GenerateUnique(func(string) bool { return false })
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
}

func TestGenerateUnique_RetriesThenSucceeds(t *testing.T) {
	wantCollisions := 5
	calls := 0
	got, err := GenerateUnique(func(string) bool {
		calls++
		return calls < wantCollisions // first wantCollisions-1 collide, the next is free
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != wantCollisions {
		t.Fatalf("attempts: got %d want %d", calls, wantCollisions)
	}
	if got == "" {
		t.Fatal("empty id")
	}
}

func TestGenerateUnique_Exhaustion(t *testing.T) {
	calls := 0
	_, err := GenerateUnique(func(string) bool {
		calls++
		return true // every candidate collides
	})
	if !errors.Is(err, ErrIDExhausted) {
		t.Fatalf("want ErrIDExhausted, got %v", err)
	}
	if calls != maxIDAttempts {
		t.Fatalf("attempts: got %d want %d", calls, maxIDAttempts)
	}
}

func contains(list []string, s string) bool {
	for _, w := range list {
		if w == s {
			return true
		}
	}
	return false
}
