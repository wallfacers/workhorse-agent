// Package delegation manages background read-only sub-agent tasks
// (001-agent-orchestration US1). A Manager spawns an ephemeral child session
// with a read-only tool surface, persists the result, and produces a one-shot
// completion notice the agent loop injects into the parent session's history.
package delegation

import (
	"crypto/rand"
	"errors"
	"math/big"
)

// ErrIDExhausted is returned when GenerateUnique cannot find an unused ID after
// maxIDAttempts collisions.
var ErrIDExhausted = errors.New("delegation: could not generate a unique id")

// maxIDAttempts bounds the collision-retry loop. 20 attempts over a 10×10×10
// (1000) space makes a pathological collision effectively impossible.
const maxIDAttempts = 20

// Human-readable delegation IDs are adjective-color-animal triples so they can
// be spoken aloud and remembered (e.g. "brisk-amber-fox"). Each list has ten
// entries; the full space is 1000 ids, plenty for a single-user store.
var (
	adjectives = []string{"brisk", "calm", "swift", "steady", "bright", "crisp", "eager", "gentle", "lively", "quiet"}
	colors     = []string{"amber", "teal", "cobalt", "coral", "indigo", "jade", "lavender", "ruby", "slate", "ochre"}
	animals    = []string{"fox", "owl", "hawk", "otter", "lynx", "heron", "fawn", "wolf", "stork", "crane"}
)

// Generate returns one random adjective-color-animal id using crypto/rand.
func Generate() (string, error) {
	adj, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	color, err := pick(colors)
	if err != nil {
		return "", err
	}
	animal, err := pick(animals)
	if err != nil {
		return "", err
	}
	return adj + "-" + color + "-" + animal, nil
}

// GenerateUnique returns an id for which exists reports false, retrying on
// collisions up to maxIDAttempts times. exists is the caller's existence probe
// (typically a store lookup). Returns ErrIDExhausted if every attempt collided.
func GenerateUnique(exists func(string) bool) (string, error) {
	for i := 0; i < maxIDAttempts; i++ {
		id, err := Generate()
		if err != nil {
			return "", err
		}
		if !exists(id) {
			return id, nil
		}
	}
	return "", ErrIDExhausted
}

func pick(words []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", err
	}
	return words[n.Int64()], nil
}
