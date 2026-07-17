package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/embedding"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// Diagnostic (opt-in): measures the chunk share of top-50 retrieval hits per
// category against a persisted bench store. Run with:
//
//	DIAG_STORE=<conv.db> DIAG_RESULTS=<results.jsonl> DIAG_CONV=0 go test -run TestChunkShareDiag -v
func TestChunkShareDiag(t *testing.T) {
	storePath := os.Getenv("DIAG_STORE")
	resultsPath := os.Getenv("DIAG_RESULTS")
	if storePath == "" || resultsPath == "" {
		t.Skip("DIAG_STORE / DIAG_RESULTS not set")
	}
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: storePath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck
	es := memory.NewEntryStore(st.DB())
	vec := memory.NewVectorStore(st.DB())
	emb, _ := embedding.New(embedding.Config{
		BaseURL: os.Getenv("EMBED_BASE_URL"), Model: os.Getenv("EMBED_MODEL"),
		APIKey: os.Getenv("EMBED_API_KEY"), Timeout: 30 * time.Second,
	})
	r := memory.NewRetriever(es, vec, emb)

	f, err := os.Open(resultsPath) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	wantConv := os.Getenv("DIAG_CONV")
	perCat := map[int][2]int{}
	dec := json.NewDecoder(f)
	for dec.More() {
		var row struct {
			Conv     int    `json:"conv"`
			Question string `json:"question"`
			Category int    `json:"category"`
		}
		if err := dec.Decode(&row); err != nil {
			break
		}
		if wantConv != "" && wantConv != itoa(row.Conv) {
			continue
		}
		hits, err := r.Search(ctx, row.Question, 50)
		if err != nil {
			continue
		}
		ch := 0
		for _, h := range hits {
			if strings.HasPrefix(h.Name, "chunk-") {
				ch++
			}
		}
		s := perCat[row.Category]
		s[0] += ch
		s[1] += len(hits)
		perCat[row.Category] = s
	}
	for c, s := range perCat {
		t.Logf("%-12s chunk-share %4d/%4d = %.0f%%", categoryLabel(c), s[0], s[1], 100*float64(s[0])/float64(s[1]))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
