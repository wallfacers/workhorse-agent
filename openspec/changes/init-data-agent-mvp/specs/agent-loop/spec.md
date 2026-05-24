## ADDED Requirements

### Requirement: LLM 推理 → 工具 → 回灌循环

会话进入 `Thinking` 状态后，Agent SHALL 反复执行下述循环直到 LLM 不再返回 tool_use 或被取消：

1. 用当前 `history` + `system prompt` + `tools schema` 调用 provider 的 `Stream`
2. 流式接收 `text_delta` / `tool_use` / `stop`，按事件类型 emit 给客户端
3. 若本轮含 `tool_use`：按并发性切批、执行（见 tool-system）、把 `tool_result[]` 追加进 history
4. 若本轮 stop 原因为 `end_turn` 或无 `tool_use`：循环结束，状态回 `Idle`
5. 若本轮被 ctx 取消：转入 `Cancelled` 收尾流程（见取消语义）

#### Scenario: 多轮工具循环

- **WHEN** 用户问 "读取 a.go 和 b.go 并总结"，LLM 返回 `[Read(a), Read(b)]`，回灌后 LLM 返回总结文本
- **THEN** Agent 完成 2 个 Read 工具的并发调用，把 2 个 tool_result 追加进 history，再调一次 LLM 拿到总结，emit `assistant_text_done` 后状态回 `Idle`

#### Scenario: 仅文本响应不触发工具循环

- **WHEN** 用户问 "你好"，LLM 返回纯文本 "你好！"，stop 原因 `end_turn`
- **THEN** Agent 不调用任何工具，emit `assistant_text_delta` / `assistant_text_done` 后状态回 `Idle`

### Requirement: 上下文自动压缩

Agent SHALL 在每次 LLM 调用前检查 `tokens_used / context_window > 0.85`；若超阈，进入 `Compacting` 状态触发压缩：

1. 用 Fast 模型（与当前 provider 同家：Anthropic → Haiku；OpenAI → `gpt-4o-mini`）总结前 N−K 条消息
2. 保留：最近 K=8 条原始消息 + 所有 `is_error=true` 的 tool_result
3. 新 history = `[summary message]` + `[recent K messages]`
4. emit `compaction { before_tokens, after_tokens }` 事件
5. 状态回 `Idle`

也 SHALL 支持手动触发：`POST /v1/sessions/{id}/compact`。

#### Scenario: 自动触发压缩

- **WHEN** 会话 token 用量达到 `170000 / 200000 = 0.85` 阈值
- **THEN** 下一次 LLM 调用前 Agent 进入 `Compacting`，调 Fast 模型总结，完成后 emit `compaction { before: 170000, after: <3500> }`

#### Scenario: 压缩保留错误 tool_result

- **WHEN** history 含 5 条 tool_result，其中 2 条 `is_error=true`，压缩窗口仅留最近 8 条但这 5 条都在 8 条之外
- **THEN** 压缩后的新 history 包含 summary、最近 8 条、以及那 2 条 error tool_result

### Requirement: 取消时半完成 tool_use 合成 cancelled tool_result

Agent SHALL 在取消时对所有"已发出 tool_use 但未收到 tool_result"的工具调用合成一条 `tool_result { tool_use_id, is_error: true, output: "<cancelled by user>" }` 追加进 history，确保 LLM 下一轮看到完整的 tool_use ↔ tool_result 配对。

半完成的 `assistant_text_delta` 累积内容 SHALL **不**写入 history（避免无头无尾的 assistant 消息）。

#### Scenario: 取消并发批中部分完成的工具

- **WHEN** LLM 返回 `[Read(a), Read(b), Bash("sleep 60")]`，并发批中 a 已完成、b 已完成、Bash 跑到一半被 cancel
- **THEN** history 追加 a 和 b 的真实 tool_result，以及为 Bash 合成的 `{ is_error: true, output: "<cancelled by user>" }`；半流式文本不入 history

#### Scenario: 取消后下一轮 LLM 可继续

- **WHEN** 上一轮被取消后用户发送新的 `user_message`
- **THEN** Agent 用包含合成 cancelled tool_result 的 history 调用 LLM，LLM 明确看到中断点，可以从中断处继续

### Requirement: Provider 错误重试与终止

Agent SHALL 对 provider 返回的可重试错误（HTTP 429、503、网络抖动）执行指数退避重试 3 次（500ms / 2s / 8s），重试期间 emit `provider_retry { attempt, after_ms }` 事件。

对不可重试错误（401、400、`context_length_exceeded` 等）SHALL 立即终止当前推理，emit `error { code, message }`，状态回 `Idle`，不入 history。

#### Scenario: 429 限流自动重试

- **WHEN** provider 返回 `429 Too Many Requests`
- **THEN** Agent 等 500ms 后重试；若仍失败，等 2s 重试；再失败等 8s 重试；3 次全失败才 emit `error`

#### Scenario: 401 直接终止

- **WHEN** provider 返回 `401 Unauthorized`
- **THEN** Agent 不重试，立即 emit `error { code: "auth_failed" }`，状态回 `Idle`
