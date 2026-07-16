package embedding_test

import (
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/embedding"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 3.14159, 1e-9, 1e9}
	b := embedding.EncodeVector(in)
	if len(b) != len(in)*4 {
		t.Fatalf("blob len %d, want %d", len(b), len(in)*4)
	}
	out, err := embedding.DecodeVector(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: %d vs %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("component %d: got %v want %v", i, out[i], in[i])
		}
	}
}

func TestDecodeBadLength(t *testing.T) {
	if _, err := embedding.DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for non-multiple-of-4 blob")
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float64
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1},
		{[]float32{1, 0}, []float32{0, 1}, 0},
		{[]float32{1, 0}, []float32{-1, 0}, -1},
		{[]float32{1, 0}, nil, 0},             // length mismatch
		{[]float32{0, 0}, []float32{1, 1}, 0}, // zero norm
	}
	for i, c := range cases {
		got := embedding.Cosine(c.a, c.b)
		if diff := got - c.want; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestTopKCosine(t *testing.T) {
	q := []float32{1, 0}
	cands := map[string][]float32{
		"same":     {1, 0},
		"orth":     {0, 1},
		"close":    {0.9, 0.1},
		"opposite": {-1, 0},
	}
	got := embedding.TopKCosine(q, cands, 2)
	if len(got) != 2 {
		t.Fatalf("expected top 2, got %d", len(got))
	}
	if got[0].Key != "same" || got[1].Key != "close" {
		t.Fatalf("ranking wrong: %+v", got)
	}
}
