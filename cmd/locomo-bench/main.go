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
// --chunks additionally indexes verbatim session chunks alongside the extracted
// facts (a chunks ∪ artifacts union store; extraction alone is lossy
// distillation — arXiv:2601.00821). --store-dir persists each conversation's
// store so later runs reuse the paid extraction pass verbatim.
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
//	EMBED_RERANK_MODEL  (optional; enables the hybrid arm's cross-encoder
//	                 rerank stage against the same EMBED_BASE_URL endpoint)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	storeDir     string
	retrieval    string
	maxConvs     int
	maxQuestions int
	topK         int
	maxTokens    int
	concurrency  int
	chunks       bool
	chunkQuota   int
	filterPool   int
	opinionPass  bool
	adversarial  int
	catTopKSpec  string
	catQuotaSpec string
	catTopK      map[int]int
	catQuota     map[int]int
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
	flag.BoolVar(&opt.chunks, "chunks", false, "union store: index verbatim session chunks alongside extracted facts (applies to every arm)")
	flag.IntVar(&opt.chunkQuota, "chunk-quota", 0, "reserve this many top-k slots for verbatim chunks (0 = pure fused order)")
	flag.IntVar(&opt.filterPool, "filter-pool", 0, "listwise LLM filter: retrieve this many candidates, one LLM call selects the relevant subset (0 = off; must exceed top-k to matter)")
	flag.StringVar(&opt.catTopKSpec, "cat-top-k", "", `per-category top-k overrides, e.g. "1=150" — multi-hop enumeration questions need evidence from many sessions`)
	flag.StringVar(&opt.catQuotaSpec, "cat-chunk-quota", "", `per-category chunk-quota overrides, e.g. "1=50,4=30"`)
	flag.BoolVar(&opt.opinionPass, "opinion-pass", false, "run a supplementary extraction pass focused on opinions/preferences/traits (ADD-only; run once per store — resuming with this flag duplicates entries)")
	flag.IntVar(&opt.adversarial, "adversarial", 0, "include category-5 adversarial questions, scored by refusal per the Mem0 convention (0 = skip, -1 = all, N = at most N per conversation)")
	flag.StringVar(&opt.storeDir, "store-dir", "", "persist per-conversation stores here and reuse their extraction on re-runs (default in-memory)")
	flag.Parse()

	if opt.dataPath == "" || opt.runDir == "" {
		flag.Usage()
		return fmt.Errorf("--data and --run-dir are required")
	}
	arms, err := armsFor(opt.retrieval)
	if err != nil {
		return err
	}
	if opt.catTopK, err = parseCatOverrides(opt.catTopKSpec); err != nil {
		return fmt.Errorf("--cat-top-k: %w", err)
	}
	if opt.catQuota, err = parseCatOverrides(opt.catQuotaSpec); err != nil {
		return fmt.Errorf("--cat-chunk-quota: %w", err)
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
	dsn := ":memory:"
	if opt.storeDir != "" {
		if err := os.MkdirAll(opt.storeDir, 0o755); err != nil {
			return fmt.Errorf("create store dir: %w", err)
		}
		dsn = filepath.Join(opt.storeDir, fmt.Sprintf("conv%d.db", conv.ID))
	}
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: dsn})
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

	// Ingest each session with its date (extraction is the shared, once-paid
	// pass). A persisted store that already holds extracted facts skips it.
	if n, err := countExtracted(ctx, es); err != nil {
		return err
	} else if n > 0 {
		logger.Info("reusing persisted extraction", "conversation", conv.ID, "facts", n)
	} else {
		for _, s := range conv.Sessions {
			msgs := make([]pipeline.Message, 0, len(s.Turns))
			for _, tn := range s.Turns {
				msgs = append(msgs, pipeline.Message{Role: "user", Text: tn.Speaker + ": " + tn.Text})
			}
			if _, err := pipe.Ingest(ctx, s.Date, fmt.Sprintf("conv%d-sess%d", conv.ID, s.Index), msgs); err != nil {
				logger.Warn("ingest session failed", "conversation", conv.ID, "session", s.Index, "err", err)
			}
		}
	}
	if opt.opinionPass {
		// Supplementary ADD-only extraction: opinions, preferences, and traits
		// are systematically under-captured by the event-focused main pass and
		// are what LoCoMo open-domain questions probe. The existing facts stay
		// untouched; this only adds entries.
		opinionPipe := pipeline.New(pipeline.Config{
			Entries:  es,
			Embedder: embedder,
			Call: func(ctx context.Context, system, user string) (string, error) {
				return extractCall(ctx, system+opinionExtractionAddendum, user)
			},
			Budgets: memory.DefaultBudgets(),
		})
		added := 0
		for _, s := range conv.Sessions {
			msgs := make([]pipeline.Message, 0, len(s.Turns))
			for _, tn := range s.Turns {
				msgs = append(msgs, pipeline.Message{Role: "user", Text: tn.Speaker + ": " + tn.Text})
			}
			n, err := opinionPipe.Ingest(ctx, s.Date, fmt.Sprintf("conv%d-sess%d-op", conv.ID, s.Index), msgs)
			if err != nil {
				logger.Warn("opinion pass failed", "conversation", conv.ID, "session", s.Index, "err", err)
				continue
			}
			added += n
		}
		logger.Info("opinion pass done", "conversation", conv.ID, "entries_added", added)
	}
	if opt.chunks {
		if n, err := ingestChunks(ctx, es, conv); err != nil {
			logger.Warn("chunk ingest failed", "conversation", conv.ID, "err", err)
		} else {
			logger.Info("verbatim chunks ingested", "conversation", conv.ID, "chunks", n)
		}
	}
	// Drain embeddings synchronously before answering (only meaningful when a
	// hybrid arm supplied an embedding client).
	if err := embedder.Backfill(ctx); err != nil {
		logger.Warn("embedding backfill failed", "conversation", conv.ID, "err", err)
	}
	embedder.Close()

	// One retriever per arm over the same store. Only the hybrid arm gets the
	// semantic signal and the optional rerank stage; fts stays the pure legacy
	// baseline.
	retrievers := make(map[string]*memory.Retriever, len(states))
	for _, s := range states {
		if s.name == "hybrid" {
			retrievers[s.name] = memory.NewRetriever(es, vectors, embClient).WithReranker(buildBenchReranker())
		} else {
			retrievers[s.name] = memory.NewRetriever(es, vectors, nil)
		}
	}

	// Answer/judge questions concurrently across arms; the global semaphore
	// bounds real in-flight LLM calls.
	var qwg sync.WaitGroup
	answered, advAnswered := 0, 0
	for qi, qa := range conv.QA {
		if qa.Category == adversarialCategory {
			if opt.adversarial == 0 || (opt.adversarial > 0 && advAnswered >= opt.adversarial) {
				continue
			}
			advAnswered++
		} else {
			if opt.maxQuestions > 0 && answered >= opt.maxQuestions {
				break
			}
			answered++
		}
		key := resultKey{Conv: conv.ID, Q: qi}
		for _, s := range states {
			if prev, ok := s.journal.lookup(key); ok {
				s.agg.add(qa.Category, prev.Correct) // resume: reuse recorded result
				continue
			}
			qwg.Add(1)
			go func(s *armState, qa locomoQA, key resultKey) {
				defer qwg.Done()
				correct, predicted := answerAndJudge(ctx, retrievers[s.name], call, opt, qa, logger)
				s.agg.add(qa.Category, correct)
				s.journal.write(result{
					Conv:      key.Conv,
					Q:         key.Q,
					Category:  qa.Category,
					Correct:   correct,
					Question:  qa.Question,
					Gold:      goldFor(qa),
					Predicted: predicted,
				})
			}(s, qa, key)
		}
	}
	qwg.Wait()
	logger.Info("conversation done", "conversation", conv.ID, "answered", answered)
	return nil
}

// opinionExtractionAddendum retargets the extraction prompt at the subjective
// layer the event-focused main pass under-captures.
const opinionExtractionAddendum = `

IMPORTANT OVERRIDE FOR THIS PASS: extract ONLY subjective facts — opinions, preferences, likes and dislikes, values, personality traits, fears, aspirations, plans, and intentions. Attribute every fact to its speaker by name (e.g. "Melanie prefers…", "Caroline believes…"). Do NOT extract plain events, dates, or activities; those are already captured. If a message contains no subjective content, extract nothing from it.`

// countExtracted reports how many non-chunk entries the store already holds,
// which signals that a persisted store's extraction pass can be reused.
func countExtracted(ctx context.Context, es *memory.EntryStore) (int, error) {
	entries, err := es.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("count extracted: %w", err)
	}
	n := 0
	for _, e := range entries {
		if e.FactSource != "verbatim_chunk" {
			n++
		}
	}
	return n, nil
}

// answerAndJudge retrieves, answers, and grades one question. When the first
// answer is an IDK bail-out, one rewrite-and-retry round runs: the model
// produces an alternative search query, its hits are unioned with the first
// round's, and the question is answered again (EverMemOS-style second round,
// paid only for the IDK tail). Returns (correct, predicted answer).
func answerAndJudge(ctx context.Context, retriever *memory.Retriever, call modelCaller, opt options, qa locomoQA, logger *slog.Logger) (bool, string) {
	topK, quota := opt.retrievalFor(qa.Category)
	hits, err := retrieve(ctx, retriever, call, qa.Question, topK, quota, opt)
	if err != nil {
		logger.Warn("retrieve failed; question scored wrong", "err", err)
		return false, ""
	}
	prompt := answerPromptFor(qa.Category)
	predicted, err := call(ctx, prompt, buildAnswerPrompt(qa.Question, toMemories(hits)))
	if err != nil {
		logger.Warn("answer call failed; question scored wrong", "err", err)
		return false, ""
	}

	if isIDK(predicted) {
		if retry, ok := retryWithRewrite(ctx, retriever, call, opt, qa, prompt, hits); ok {
			predicted = retry
		} else if retry, ok := retryWithWiderNet(ctx, retriever, call, opt, qa, prompt); ok {
			predicted = retry
		}
	}

	verdict, err := call(ctx, judgeSystemPrompt, buildJudgePrompt(qa.Question, goldFor(qa), predicted))
	if err != nil {
		logger.Warn("judge call failed; question scored wrong", "err", err)
		return false, predicted
	}
	return parseJudgeVerdict(verdict), predicted
}

// adversarialGold is the judge-facing gold for category-5 questions. They have
// no answer in the conversation (the dataset's adversarial_answer field is the
// TRAP, not the truth) — per the Mem0/LoCoMo convention a correct response
// declines to answer.
const adversarialGold = `This question cannot be answered from the conversation — it contains no such information. The correct response DECLINES: it says the information is not mentioned, not available, or "I don't know". Any confident substantive answer is wrong.`

func goldFor(qa locomoQA) string {
	if qa.Category == adversarialCategory {
		return adversarialGold
	}
	return qa.AnswerText()
}

// parseCatOverrides parses "cat=value" pairs ("1=150,4=30") into a map.
func parseCatOverrides(spec string) (map[int]int, error) {
	m := map[int]int{}
	if spec == "" {
		return m, nil
	}
	for _, part := range strings.Split(spec, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("expected cat=value, got %q", part)
		}
		c, err1 := strconv.Atoi(strings.TrimSpace(kv[0]))
		v, err2 := strconv.Atoi(strings.TrimSpace(kv[1]))
		if err1 != nil || err2 != nil || c < 1 || v < 1 {
			return nil, fmt.Errorf("invalid pair %q", part)
		}
		m[c] = v
	}
	return m, nil
}

// retrievalFor resolves the per-question retrieval budget; categories with an
// override (e.g. multi-hop enumeration needs breadth) diverge from the global
// defaults.
func (o options) retrievalFor(category int) (topK, quota int) {
	topK, quota = o.topK, o.chunkQuota
	if v, ok := o.catTopK[category]; ok {
		topK = v
	}
	if v, ok := o.catQuota[category]; ok {
		quota = v
	}
	return topK, quota
}

// retrieve is the per-question retrieval front door: quota'd top-k, optionally
// widened + narrowed by the listwise LLM filter when --filter-pool is set.
func retrieve(ctx context.Context, retriever *memory.Retriever, call modelCaller, query string, topK, quota int, opt options) ([]memory.Result, error) {
	if opt.filterPool > topK {
		return retrieveFiltered(ctx, retriever, call, query, topK, quota, opt.filterPool)
	}
	return retrieveWithQuota(ctx, retriever, query, topK, quota)
}

// retryWithRewrite runs the IDK second round. Returns (answer, true) only when
// the retry produced a non-IDK answer worth keeping.
func retryWithRewrite(ctx context.Context, retriever *memory.Retriever, call modelCaller, opt options, qa locomoQA, prompt string, first []memory.Result) (string, bool) {
	rewritten, err := call(ctx, queryRewriteSystemPrompt, "QUESTION: "+qa.Question)
	if err != nil {
		return "", false
	}
	rewritten = strings.TrimSpace(rewritten)
	if rewritten == "" || rewritten == qa.Question {
		return "", false
	}
	topK, quota := opt.retrievalFor(qa.Category)
	more, err := retrieve(ctx, retriever, call, rewritten, topK, quota, opt)
	if err != nil || len(more) == 0 {
		return "", false
	}
	seen := make(map[string]struct{}, len(first))
	union := make([]memory.Result, 0, len(first)+len(more))
	for _, h := range first {
		seen[h.Name] = struct{}{}
		union = append(union, h)
	}
	fresh := 0
	for _, h := range more {
		if _, dup := seen[h.Name]; dup {
			continue
		}
		union = append(union, h)
		fresh++
	}
	if fresh == 0 {
		return "", false
	}
	retry, err := call(ctx, prompt, buildAnswerPrompt(qa.Question, toMemories(union)))
	if err != nil || isIDK(retry) {
		return "", false
	}
	return retry, true
}

// retryWithWiderNet is the second-stage IDK escalation: when the rewrite round
// also failed, re-retrieve the ORIGINAL question at 3× breadth and answer once
// more. It only ever fires on the IDK tail, so an aggressive net is safe — any
// grounded answer beats a bail-out. Returns (answer, true) only on a non-IDK
// answer.
func retryWithWiderNet(ctx context.Context, retriever *memory.Retriever, call modelCaller, opt options, qa locomoQA, prompt string) (string, bool) {
	topK, quota := opt.retrievalFor(qa.Category)
	hits, err := retrieveWithQuota(ctx, retriever, qa.Question, topK*3, quota*3)
	if err != nil || len(hits) <= topK {
		return "", false
	}
	retry, err := call(ctx, prompt, buildAnswerPrompt(qa.Question, toMemories(hits)))
	if err != nil || isIDK(retry) {
		return "", false
	}
	return retry, true
}

// toMemories converts retrieval hits into the prompt-facing form.
func toMemories(hits []memory.Result) []retrievedMemory {
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
	return mems
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

// buildBenchReranker builds the rerank client from EMBED_RERANK_MODEL (empty =
// disabled) against the same EMBED_BASE_URL endpoint.
func buildBenchReranker() embedding.Reranker {
	rr, err := embedding.NewReranker(embedding.RerankConfig{
		BaseURL: envOr("EMBED_BASE_URL", "http://127.0.0.1:11434/v1"),
		Model:   os.Getenv("EMBED_RERANK_MODEL"),
		APIKey:  os.Getenv("EMBED_API_KEY"),
		Timeout: 60 * time.Second,
	})
	if err != nil || rr == nil {
		return nil
	}
	return rr
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
