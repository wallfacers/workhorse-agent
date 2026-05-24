## ADDED Requirements

### Requirement: Provider 接口

服务 SHALL 定义统一 Provider 接口：

```go
type Provider interface {
    Name() string
    Stream(ctx context.Context, req Request) (<-chan ProviderEvent, error)
}
```

`Stream` 返回事件 channel，依次产生 `text_delta`、`tool_use`、`stop`、`usage`、`error` 事件。channel 在响应完成或 ctx 取消后 close。

#### Scenario: provider channel 在响应结束后 close

- **WHEN** Provider.Stream 完成一次正常响应（含 stop）
- **THEN** 返回的 channel 在 stop 事件之后被 close；下游 range 退出

#### Scenario: ctx 取消时 channel close

- **WHEN** Stream 进行中 ctx 被 cancel
- **THEN** Provider 中止 HTTP 请求，channel 立即 close（可能伴随一个 `error { Err: ctx.Err() }` 事件）

### Requirement: 内部统一 Message 格式

服务 SHALL 维护内部 `Message` / `ContentBlock` 类型，语义对齐 Anthropic Messages（`role + blocks[]`，blocks 含 `text`/`tool_use`/`tool_result`）。所有 history 操作、压缩、持久化均以此格式为准。

#### Scenario: tool_use 与 tool_result 配对

- **WHEN** 一条 assistant message 含 `tool_use { id: "abc" }`
- **THEN** 紧随其后的 user message SHALL 含对应 `tool_result { tool_use_id: "abc", output }`

### Requirement: Anthropic adapter

服务 SHALL 提供 `anthropic` provider，对接 `https://api.anthropic.com/v1/messages` SSE 端点。

adapter SHALL：
- 直接映射内部 Message → Anthropic Messages 请求
- 解析 Anthropic SSE 事件流（`message_start`、`content_block_start`、`content_block_delta`、`content_block_stop`、`message_delta`、`message_stop`、`ping`、`error`）
- 将 `content_block_delta.text_delta` 翻成内部 `text_delta` 事件
- 将 `tool_use` 块累积完成后翻成内部 `tool_use` 事件
- 从 `message_delta.usage` 提取 input/output token 数

#### Scenario: 流式接收 Anthropic SSE

- **WHEN** Anthropic 返回 SSE 流含 5 个 text delta + 1 个 tool_use + message_stop
- **THEN** adapter 通过 channel 依次 emit 5 个 `text_delta`、1 个 `tool_use`、1 个 `stop` 事件

### Requirement: OpenAI adapter

服务 SHALL 提供 `openai` provider，对接 OpenAI Chat Completions API（默认 `https://api.openai.com/v1/chat/completions`，`base_url` 可配以接 OpenAI 兼容服务）。

adapter SHALL：
- 把内部 `tool_use` block 翻成 `assistant.tool_calls[]`
- 把内部 `tool_result` block 翻成独立的 `{ role: "tool", tool_call_id, content }` 消息
- 在内部 history 含同一 assistant 消息既有 text 又有 tool_use 时，先发 text，再单独发 tool_calls（OpenAI 不允许交错）
- 累积 SSE `delta.tool_calls` 流；在 `finish_reason: "tool_calls"` 时 emit 完整 `tool_use` 事件

#### Scenario: tool_result 翻译为独立 tool 消息

- **WHEN** 内部 history 末尾的 user message 含 1 个 tool_result
- **THEN** adapter 在 OpenAI 请求中追加一条 `{ role: "tool", tool_call_id: <id>, content: <output> }`，不放进 user 消息

### Requirement: 兼容范围声明

服务 SHALL 在文档中声明：
- 官方测试通过：Anthropic Messages API、OpenAI 官方 Chat Completions API
- OpenAI 兼容端（DeepSeek、Qwen、豆包、Ollama 等）可通过 `base_url` 接入，**不保证**、**不维护**

#### Scenario: 文档明示

- **WHEN** 阅读 README 的"Provider Support"章节
- **THEN** 该章节明确列出官方测试 vs 仅技术上兼容的范围

### Requirement: 模型选择策略

服务 SHALL 实现 `ModelPolicy`：

```go
type ModelPolicy struct {
    Default       string  // 如 "anthropic:claude-sonnet-4-6"
    Fast          string  // 如 "anthropic:claude-haiku-4-5"，用于压缩与小任务
    BySessionType map[string]string
}
```

会话/agent_type 显式指定 model 时优先；缺省走 ModelPolicy。

Fast 模型 SHALL 遵循**同家原则**：当前会话 provider 是 Anthropic → Fast 用 Anthropic 系（Haiku）；OpenAI → Fast 用 OpenAI 系（`gpt-4o-mini`）。

#### Scenario: 同家压缩

- **WHEN** OpenAI session 触发压缩
- **THEN** 压缩调用使用 `gpt-4o-mini` 而非跨家调用 Anthropic Haiku

### Requirement: 不引入第三方 LLM SDK

服务 SHALL 自实现 Anthropic / OpenAI 的 HTTP 客户端（POST + SSE 解析）；**不**引入 `anthropic-sdk-go`、`sashabaranov/go-openai` 等 SDK 依赖。

理由：SDK 演进快易导致依赖锁定；自实现易于合规留痕；薄客户端工程量小。

#### Scenario: go.mod 不含 LLM SDK

- **WHEN** 检视 `go.mod` 中的 direct dependencies
- **THEN** 不含 `github.com/anthropics/anthropic-sdk-go`、`github.com/sashabaranov/go-openai` 等
