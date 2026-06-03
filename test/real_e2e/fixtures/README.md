# Real E2E Test Fixtures

## recordings/
JSONL files containing recorded LLM interactions. Each file is named after the
test case and contains a header line + one JSON line per Stream() call.

To regenerate: `WORKHORSE_TEST_MODE=record go test ./test/real_e2e/... -tags=real_e2e -run <TestName>`

## judge_cache/
Cached Judge evaluation results. Keyed by SHA-256 hash of (normalized trace +
rubric). The trace is normalized before hashing — per-turn `Duration` (wall
clock) is zeroed — so the key is stable across record/replay runs. Commit a
cache entry and the CI path (`WORKHORSE_TEST_MODE=replay
WORKHORSE_JUDGE_MODE=cached`, no API key) hits it and asserts the judged
verdict instead of skipping.

To regenerate a cache entry (needs a key for the judge; the agent replays):
`WORKHORSE_TEST_MODE=replay WORKHORSE_JUDGE_MODE=llm DASHSCOPE_API_KEY=… go test ./test/real_e2e/... -tags=real_e2e -run <TestName>`
