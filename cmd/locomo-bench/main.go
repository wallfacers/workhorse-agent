// Command locomo-bench evaluates the memory subsystem on the LoCoMo benchmark
// (memory-hybrid-retrieval-locomo). It ingests each conversation through the
// ADD-only extraction pipeline into a throwaway store, answers each question in
// a single pass from the top-k retrieval results, and scores answers with an
// LLM-as-a-Judge aligned with the open mem0ai/memory-benchmarks methodology.
//
// The --retrieval flag switches the backend (fts | hybrid | both). "both" runs
// the two retrievers over ONE shared extraction so the semantic signal's uplift
// is measured A-B under identical extraction, answering, and judging — and the
// costly extraction pass is paid once, not twice. Runs are resumable via a
// per-arm JSONL artifact and parallelized with a global LLM-call semaphore.
//
// Credentials come from the environment only and are never logged or written to
// run artifacts:
//
//	LOCOMO_API_KEY   (required) answer + judge model key
//	LOCOMO_BASE_URL  (default https://api.deepseek.com/anthropic)
//	LOCOMO_MODEL     (default deepseek-v4-pro)     answer + judge model
//	EXTRACT_MODEL    (default = LOCOMO_MODEL)      extraction model (a fast,
//	                 non-reasoning model here cuts wall-clock and cost markedly)
//	EMBED_API_KEY / EMBED_BASE_URL / EMBED_MODEL  (hybrid arm embedding client)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/embedding"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/memory/pipeline"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

type options struct {
	dataPath     string
	runDir       string
	retrieval    string
	maxConvs     int
	maxQuestions int
	topK         int
	maxTokens    int
	concurrency  int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "locomo-bench:", err)
		os.Exit(1)
	}
}

func run() error {
	var opt options
	flag.StringVar(&opt.dataPath, "data", "", "path to LoCoMo JSON dataset (required)")
	flag.StringVar(&opt.runDir, "run-dir", "", "directory for resumable JSONL run artifacts (required)")
	flag.StringVar(&opt.retrieval, "retrieval", "both", "retrieval backend: fts | hybrid | both")
	flag.IntVar(&opt.maxConvs, "conversations", 0, "limit number of conversations (0 = all)")
	flag.IntVar(&opt.maxQuestions, "questions", 0, "limit questions per conversation (0 = all)")
	flag.IntVar(&opt.topK, "top-k", 30, "retrieval budget per question")
	flag.IntVar(&opt.maxTokens, "max-tokens", 8000, "max output tokens (reasoning models need headroom for thinking + answer)")
	flag.IntVar(&opt.concurrency, "concurrency", 24, "max concurrent in-flight LLM calls")
	flag.Parse()

	if opt.dataPath == "" || opt.runDir == "" {
		flag.Usage()
		return fmt.Errorf("--data and --run-dir are required")
	}
	arms, err := armsFor(opt.retrieval)
	if err != nil {
		return err
	}
	if opt.concurrency < 1 {
		opt.concurrency = 1
	}

	apiKey := os.Getenv("LOCOMO_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("LOCOMO_API_KEY is required (never passed as a flag so it stays out of process listings)")
	}
	baseURL := envOr("LOCOMO_BASE_URL", "https://api.deepseek.com/anthropic")
	model := envOr("LOCOMO_MODEL", "deepseek-v4-pro")
	extractModel := envOr("EXTRACT_MODEL", model)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(opt.runDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}

	convs, err := loadDataset(opt.dataPath)
	if err != nil {
		return err
	}
	if opt.maxConvs > 0 && opt.maxConvs < len(convs) {
		convs = convs[:opt.maxConvs]
		logger.Info("sampling conversations", "limit", opt.maxConvs)
	}

	// One provider; a global semaphore caps concurrent in-flight LLM calls so
	// many conversations/questions run in parallel without exceeding the rate
	// limit. Every LLM call (extraction, answer, judge) passes through it.
	prov := anthropic.New(anthropic.Options{APIKey: apiKey, BaseURL: baseURL, DefaultMaxTokens: opt.maxTokens})
	sem := make(chan struct{}, opt.concurrency)
	call := gate(sem, newModelCaller(prov, model, opt.maxTokens))
	extractCall := pipeline.ModelCaller(gate(sem, newModelCaller(prov, extractModel, opt.maxTokens)))

	// Per-arm aggregator + resumable journal.
	states := make([]*armState, 0, len(arms))
	for _, name := range arms {
		j, err := openJournal(opt.runDir, name)
		if err != nil {
			return err
		}
		defer j.Close()
		states = append(states, &armState{name: name, agg: newAggregator(), journal: j})
	}

	// The embedding client is shared across conversations (safe for concurrent
	// use) and only built when a hybrid arm is present.
	var embClient embedding.Client
	if hasArm(arms, "hybrid") {
		embClient = buildBenchEmbeddingClient(logger)
	}

	logger.Info("starting", "conversations", len(convs), "arms", arms, "concurrency", opt.concurrency,
		"model", model, "extract_model", extractModel, "top_k", opt.topK)

	ctx := context.Background()
	var wg sync.WaitGroup
	for ci := range convs {
		wg.Add(1)
		go func(conv conversation) {
			defer wg.Done()
			if err := processConversation(ctx, opt, conv, extractCall, call, embClient, states, logger); err != nil {
				logger.Warn("conversation failed", "conversation", conv.ID, "err", err)
			}
		}(convs[ci])
	}
	wg.Wait()

	for _, s := range states {
		report(s, opt)
	}
	if len(states) == 2 {
		reportDelta(states[0], states[1])
	}
	return nil
}

// armState holds one retrieval arm's grading state.
type armState struct {
	name    string
	agg     *aggregator
	journal *journal
}

func armsFor(retrieval string) ([]string, error) {
	switch retrieval {
	case "fts", "hybrid":
		return []string{retrieval}, nil
	case "both":
		return []string{"fts", "hybrid"}, nil
	default:
		return nil, fmt.Errorf("--retrieval must be fts, hybrid, or both, got %q", retrieval)
	}
}

func hasArm(arms []string, name string) bool {
	for _, a := range arms {
		if a == name {
			return true
		}
	}
	return false
}

// gate wraps a modelCaller so each call holds one slot of the global semaphore
// for its full duration — the true in-flight-call limit.
func gate(sem chan struct{}, c modelCaller) modelCaller {
	return func(ctx context.Context, system, user string) (string, error) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-sem }()
		return c(ctx, system, user)
	}
}

// processConversation ingests one conversation ONCE, then answers/judges its
// questions under every retrieval arm from that shared store. Extraction is
// sequential (the store is single-connection); questions run concurrently and
// are bounded by the global LLM-call semaphore.
func processConversation(ctx context.Context, opt options, conv conversation, extractCall pipeline.ModelCaller, call modelCaller, embClient embedding.Client, states []*armState, logger *slog.Logger) error {
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close() //nolint:errcheck

	es := memory.NewEntryStore(st.DB())
	vectors := memory.NewVectorStore(st.DB())
	embedder := memory.NewEmbedder(es, vectors, embClient, memory.DefaultEmbedBuffer)

	pipe := pipeline.New(pipeline.Config{
		Entries:  es,
		Embedder: embedder,
		Call:     extractCall,
		Budgets:  memory.DefaultBudgets(),
	})

	// Ingest each session with its date (extraction is the shared, once-paid pass).
	for _, s := range conv.Sessions {
		msgs := make([]pipeline.Message, 0, len(s.Turns))
		for _, tn := range s.Turns {
			msgs = append(msgs, pipeline.Message{Role: "user", Text: tn.Speaker + ": " + tn.Text})
		}
		if _, err := pipe.Ingest(ctx, s.Date, fmt.Sprintf("conv%d-sess%d", conv.ID, s.Index), msgs); err != nil {
			logger.Warn("ingest session failed", "conversation", conv.ID, "session", s.Index, "err", err)
		}
	}
	// Drain embeddings synchronously before answering (only meaningful when a
	// hybrid arm supplied an embedding client).
	if err := embedder.Backfill(ctx); err != nil {
		logger.Warn("embedding backfill failed", "conversation", conv.ID, "err", err)
	}
	embedder.Close()

	// One retriever per arm over the same store.
	retrievers := make(map[string]*memory.Retriever, len(states))
	for _, s := range states {
		if s.name == "hybrid" {
			retrievers[s.name] = memory.NewRetriever(es, vectors, embClient)
		} else {
			retrievers[s.name] = memory.NewRetriever(es, vectors, nil)
		}
	}

	// Answer/judge questions concurrently across arms; the global semaphore
	// bounds real in-flight LLM calls.
	var qwg sync.WaitGroup
	answered := 0
	for qi, qa := range conv.QA {
		if qa.Category == adversarialCategory {
			continue
		}
		if opt.maxQuestions > 0 && answered >= opt.maxQuestions {
			break
		}
		answered++
		key := resultKey{Conv: conv.ID, Q: qi}
		for _, s := range states {
			if prev, ok := s.journal.lookup(key); ok {
				s.agg.add(qa.Category, prev.Correct) // resume: reuse recorded result
				continue
			}
			qwg.Add(1)
			go func(s *armState, qa locomoQA, key resultKey) {
				defer qwg.Done()
				correct, predicted := answerAndJudge(ctx, retrievers[s.name], call, opt.topK, qa)
				s.agg.add(qa.Category, correct)
				s.journal.write(result{
					Conv:      key.Conv,
					Q:         key.Q,
					Category:  qa.Category,
					Correct:   correct,
					Question:  qa.Question,
					Gold:      qa.AnswerText(),
					Predicted: predicted,
				})
			}(s, qa, key)
		}
	}
	qwg.Wait()
	logger.Info("conversation done", "conversation", conv.ID, "answered", answered)
	return nil
}

// answerAndJudge retrieves, answers, and grades one question. Returns (correct,
// predicted answer).
func answerAndJudge(ctx context.Context, retriever *memory.Retriever, call modelCaller, topK int, qa locomoQA) (bool, string) {
	hits, err := retriever.Search(ctx, qa.Question, topK)
	if err != nil {
		return false, ""
	}
	mems := make([]retrievedMemory, 0, len(hits))
	for _, h := range hits {
		rm := retrievedMemory{Content: h.Content}
		if h.EventDate != nil && !h.EventDate.IsZero() {
			rm.EventDate = h.EventDate.Format("2006-01-02")
		}
		if !h.CreatedAt.IsZero() {
			rm.Recorded = h.CreatedAt.Format("2006-01-02")
		}
		mems = append(mems, rm)
	}
	predicted, err := call(ctx, answerSystemPrompt, buildAnswerPrompt(qa.Question, mems))
	if err != nil {
		return false, ""
	}
	verdict, err := call(ctx, judgeSystemPrompt, buildJudgePrompt(qa.Question, qa.AnswerText(), predicted))
	if err != nil {
		return false, predicted
	}
	return parseJudgeVerdict(verdict), predicted
}

// buildBenchEmbeddingClient builds the embedding client from EMBED_* env, with
// local defaults. Returns nil (semantic disabled) on failure.
func buildBenchEmbeddingClient(logger *slog.Logger) embedding.Client {
	c, err := embedding.New(embedding.Config{
		BaseURL: envOr("EMBED_BASE_URL", "http://127.0.0.1:11434/v1"),
		Model:   envOr("EMBED_MODEL", "qwen3-embedding:0.6b"),
		APIKey:  os.Getenv("EMBED_API_KEY"),
		Timeout: 30 * time.Second,
	})
	if err != nil || c == nil {
		logger.Warn("hybrid arm: embedding client unavailable; semantic signal disabled (degrades to BM25+entity)")
		return nil
	}
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---- aggregation + report ----

type aggregator struct {
	mu         sync.Mutex
	byCategory map[int]*catStat
}

type catStat struct {
	total, correct int
}

func newAggregator() *aggregator { return &aggregator{byCategory: map[int]*catStat{}} }

func (a *aggregator) add(category int, correct bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.byCategory[category]
	if s == nil {
		s = &catStat{}
		a.byCategory[category] = s
	}
	s.total++
	if correct {
		s.correct++
	}
}

func (a *aggregator) overall() (correct, total int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.byCategory {
		correct += s.correct
		total += s.total
	}
	return correct, total
}

func report(s *armState, opt options) {
	a := s.agg
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Printf("\n=== LoCoMo results (retrieval=%s, top_k=%d) ===\n", s.name, opt.topK)
	cats := make([]int, 0, len(a.byCategory))
	for c := range a.byCategory {
		cats = append(cats, c)
	}
	sort.Ints(cats)
	var total, correct int
	for _, c := range cats {
		st := a.byCategory[c]
		total += st.total
		correct += st.correct
		fmt.Printf("  %-14s %4d/%4d  %5.1f%%\n", categoryLabel(c), st.correct, st.total, pct(st.correct, st.total))
	}
	fmt.Printf("  %-14s %4d/%4d  %5.1f%%\n", "OVERALL (J)", correct, total, pct(correct, total))
	if opt.maxConvs > 0 || opt.maxQuestions > 0 {
		fmt.Printf("  (sampled run: conversations=%d questions/conv=%d)\n", opt.maxConvs, opt.maxQuestions)
	}
}

// reportDelta prints the A-B uplift between two arms (typically fts vs hybrid).
func reportDelta(a, b *armState) {
	ac, at := a.agg.overall()
	bc, bt := b.agg.overall()
	fmt.Printf("\n=== A-B uplift (%s → %s) ===\n", a.name, b.name)
	fmt.Printf("  %-8s J = %5.1f%%\n", a.name, pct(ac, at))
	fmt.Printf("  %-8s J = %5.1f%%\n", b.name, pct(bc, bt))
	fmt.Printf("  delta       %+5.1f pp\n", pct(bc, bt)-pct(ac, at))
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}
