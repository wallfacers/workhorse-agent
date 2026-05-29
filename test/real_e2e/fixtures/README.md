# Real E2E Test Fixtures

## recordings/
JSONL files containing recorded LLM interactions. Each file is named after the
test case and contains a header line + one JSON line per Stream() call.

To regenerate: `WORKHORSE_TEST_MODE=record go test ./test/real_e2e/... -tags=real_e2e -run <TestName>`

## judge_cache/
Cached Judge (GLM-5) evaluation results. Keyed by SHA-256 hash of (trace + rubric).

To regenerate: `WORKHORSE_JUDGE_MODE=llm go test ./test/real_e2e/... -tags=real_e2e -run <TestName>`
