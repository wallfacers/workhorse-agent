# agent-loop Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
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

Agent SHALL 在取消时对所有"已发出 tool_use 但未收到 tool_result"的工具调用合成一条 `tool_result { tool_use_id, is_error: true, output: "[CANCELLED] Tool execution was interrupted by user" }` 追加进 history，确保 LLM 下一轮看到完整的 tool_use ↔ tool_result 配对。

system prompt SHALL 包含一段说明告知 LLM `[CANCELLED]` 前缀的语义（用户主动中断，不必重试该工具调用），避免 LLM 把这条字符串当作普通用户输入解读。

半完成的 `assistant_text_delta` 累积内容 SHALL **不**写入 history（避免无头无尾的 assistant 消息）。

#### Scenario: 取消并发批中部分完成的工具

- **WHEN** LLM 返回 `[Read(a), Read(b), Bash("sleep 60")]`，并发批中 a 已完成、b 已完成、Bash 跑到一半被 cancel
- **THEN** history 追加 a 和 b 的真实 tool_result，以及为 Bash 合成的 `{ is_error: true, output: "[CANCELLED] Tool execution was interrupted by user" }`；半流式文本不入 history

#### Scenario: 取消后下一轮 LLM 可继续

- **WHEN** 上一轮被取消后用户发送新的 `user_message`
- **THEN** Agent 用包含合成 cancelled tool_result 的 history 调用 LLM，LLM 明确看到中断点，可以从中断处继续

### Requirement: Provider 错误重试与终止

Agent SHALL 依靠 `provider-abstraction` capability 的 `ProviderError.IsRetryable()` 判断重试，而非自行解析 HTTP 状态码。

对 `IsRetryable() == true` 的错误（`rate_limited`、`server_error`、`network_error`、`stream_broken`）执行指数退避重试 3 次（500ms / 2s / 8s，可配 `agent.provider_retry_backoff_ms`）；若 `ProviderError.RetryAfter()` 给定值大于退避值，SHALL 取 `RetryAfter()`。重试期间 emit `provider_retry { attempt, after_ms, code }` 事件（由 `api-protocol` 11 种事件之一承载）。

对 `IsRetryable() == false` 的错误（`auth_failed`、`invalid_request`、`context_length_exceeded`、`insufficient_quota`、`canceled`）SHALL 立即终止当前推理，emit `error { code, message }`，状态回 `Idle`，不入 history。

#### Scenario: 429 限流自动重试

- **WHEN** provider 返回 `429 Too Many Requests`
- **THEN** Agent 等 500ms 后重试；若仍失败，等 2s 重试；再失败等 8s 重试；3 次全失败才 emit `error`

#### Scenario: 401 直接终止

- **WHEN** provider 返回 `401 Unauthorized`
- **THEN** Agent 不重试，立即 emit `error { code: "auth_failed" }`，状态回 `Idle`

### Requirement: 系统提示词由 prompt 包集中管理

Agent loop SHALL 通过 `internal/prompt` 包的 `BuildSystemPrompt(base string) string`
获取系统提示词，而非自行拼接。`prompt` 包统一管理：

- `CancelledToolOutput` 常量 — 取消时合成的 tool_result 输出
- `CancelledNote` 常量 — cancelled 标记的语义说明（不含前导换行）
- `SystemPrompt` 模板 — 带 `{{.BasePrompt}}` 占位符，由 `{{if .BasePrompt}}\n\n{{end}}`
  控制前导换行，自动附加 CancelledNote
- `Compaction` 模板 — 上下文压缩摘要器的系统提示词
- `CompactionFallback` 常量 — 摘要失败时的兜底文本

Agent loop 中所有对上述常量和函数的引用 SHALL 通过 `prompt.` 前缀访问。

`Compactor` struct 的 `SystemPrompt string` 字段（从未被读取的死字段）SHALL 被删除。

#### Scenario: BuildSystemPrompt 输出等价

- **WHEN** `BuildSystemPrompt("")` 被调用
- **THEN** 返回纯 CancelledNote（无前导空行），与迁移前行为逐字节一致

- **WHEN** `BuildSystemPrompt("You are a helper.")` 被调用
- **THEN** 返回 `"You are a helper.\n\n" + CancelledNote`，与迁移前行为逐字节一致

### Requirement: System prompt 组装顺序优先静态前缀

Agent loop 在构造每个 turn 的 system prompt 时 SHALL 以**静态 base 段**（由
`prompt.BuildSystemPrompt` 渲染的 `DefaultBasePrompt` 或会话自带 `system_prompt`，
含 `CancelledNote`）作为**前缀**，再依次追加动态的 `<environment>` 块与 memory 块。
最终组装顺序 SHALL 为 `base → environment → memory`，使最稳定的内容落在 Anthropic
prompt-cache 的前缀区，最大化跨会话/跨 turn 的缓存命中。

组装路径 SHALL 唯一（不存在两套拼接实现），且在相同输入下输出 byte-stable，以便
缓存前缀在同会话各 turn 间保持逐字节一致。

#### Scenario: 静态 base 作为缓存前缀

- **WHEN** 一个会话的 base、`<environment>`、memory 三段均非空
- **THEN** 渲染出的 system prompt 以 base 段开头，`<environment>` 块紧随其后，
  memory 块位于最末，三段之间以稳定分隔符连接

#### Scenario: 仅有 base 段

- **WHEN** 会话的 `<environment>` 与 memory 均为空
- **THEN** system prompt 等于 base 段（含 `CancelledNote`），不含任何
  environment/memory 相关的框架文字或多余分隔符

#### Scenario: 顺序变更不改变内容集合

- **WHEN** 对比顺序调整前后、相同输入下渲染的 system prompt
- **THEN** 两者包含的文本片段集合完全一致，仅片段排列顺序不同，对模型语义无影响

