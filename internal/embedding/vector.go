package embedding

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// EncodeVector serializes a float32 vector as a little-endian BLOB (4 bytes per
// component), the storage form for memory_embeddings.vec.
func EncodeVector(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// DecodeVector parses a little-endian float32 BLOB produced by EncodeVector.
func DecodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("embedding: vector blob length %d not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// Cosine returns the cosine similarity of a and b in [-1, 1]. Mismatched lengths
// or a zero-norm vector yield 0 (treated as "no signal" rather than an error, so
// a single bad row cannot break a whole search).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Scored pairs an item key with a similarity score.
type Scored struct {
	Key   string
	Score float64
}

// TopKCosine ranks candidates by cosine similarity to query and returns the top
// k (descending score, key ascending as a deterministic tiebreak). k <= 0
// returns all ranked candidates.
func TopKCosine(query []float32, candidates map[string][]float32, k int) []Scored {
	scored := make([]Scored, 0, len(candidates))
	for key, vec := range candidates {
		scored = append(scored, Scored{Key: key, Score: Cosine(query, vec)})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Key < scored[j].Key
	})
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	return scored
}
