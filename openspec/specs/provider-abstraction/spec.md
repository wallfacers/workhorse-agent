# provider-abstraction Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: Provider 接口

服务 SHALL 定义统一 Provider 接口：

```go
type Provider interface {
    Name() string
    Stream(ctx context.Context, req Request) (<-chan ProviderEvent, error)
}
```

`Stream` 的返回值语义 SHALL 严格区分：

- 返回的 **`error` ≠ nil**：表示"请求**未发出**"或"HTTP 请求即失败"（如 ctx 已 cancel、URL 配置错误、TLS 握手失败、HTTP 4xx 在第一字节前返回）。此时 channel 为 nil 或已关闭。调用方 SHALL 把这类 error 直接 wrap 成 `ProviderError` 判断重试。
- 返回的 **error == nil**：表示"请求已发出且 HTTP 200，正在 stream"。后续错误通过 channel 上的 `error` 事件传递。调用方 SHALL 通过 channel range 消费事件。

channel 在以下任一情况后 SHALL close：(a) Provider 收到 `stop` 事件、(b) ctx 被 cancel、(c) HTTP 连接断开。close 前 SHALL emit 一条对应的 `stop` 或 `error` 事件作为流终点。

#### Scenario: Stream 直接返回 error（请求未发出）

- **WHEN** 调用 Stream 时 ctx 已 cancel
- **THEN** 返回 `(nil, context.Canceled)`；调用方知道请求根本未发出

#### Scenario: HTTP 200 后 SSE 中错误

- **WHEN** Provider 返回 200 + SSE 流，stream 中途网络断开
- **THEN** Stream 已返回 `(channel, nil)`；channel 上 emit `error { Type: "stream_broken", Err: ... }` 后 close

#### Scenario: provider channel 在响应结束后 close

- **WHEN** Provider.Stream 完成一次正常响应（含 stop）
- **THEN** 返回的 channel 在 stop 事件之后被 close；下游 range 退出

#### Scenario: ctx 取消时 channel close

- **WHEN** Stream 进行中 ctx 被 cancel
- **THEN** Provider 中止 HTTP 请求，channel emit `error { Err: ctx.Err() }` 后 close

### Requirement: ProviderError 与可重试分类

服务 SHALL 在 `internal/provider` 定义统一 ProviderError 类型与可重试分类，集中在 provider 层而非 agent 层判断：

```go
type ProviderError struct {
    Provider   string  // "anthropic" | "openai"
    StatusCode int     // HTTP 状态码（0 表示非 HTTP 错误）
    Code       string  // "rate_limited" | "auth_failed" | "context_length_exceeded" |
                       // "insufficient_quota" | "invalid_request" | "server_error" |
                       // "network_error" | "stream_broken" | "canceled"
    Message    string
    Cause      error
}

func (e *ProviderError) IsRetryable() bool
func (e *ProviderError) RetryAfter() time.Duration  // 解析 Retry-After header，无则零
```

每个 adapter SHALL 把自家 HTTP 状态码与错误体翻译成统一 ProviderError：

| HTTP / 场景 | Code | IsRetryable |
|---|---|---|
| 429 / Anthropic `rate_limit_error` / OpenAI `rate_limit_exceeded` | `rate_limited` | true |
| 503 / 502 / 504 | `server_error` | true |
| 网络抖动 / EOF / DNS / TLS | `network_error` | true |
| SSE 流中途断开 | `stream_broken` | true |
| 401 / Anthropic `authentication_error` / OpenAI `invalid_api_key` | `auth_failed` | false |
| 400 / Anthropic `invalid_request_error` | `invalid_request` | false |
| Anthropic `context_length_exceeded` / OpenAI 同 | `context_length_exceeded` | false |
| OpenAI `insufficient_quota` | `insufficient_quota` | false |
| ctx 取消 | `canceled` | false |

#### Scenario: 429 标记可重试

- **WHEN** Anthropic 返回 `HTTP 429` 含 `Retry-After: 5`
- **THEN** Provider 返回 `(nil, &ProviderError{Code:"rate_limited", StatusCode:429})`；`IsRetryable()` 返回 true；`RetryAfter()` 返回 5s

#### Scenario: 401 不可重试

- **WHEN** OpenAI 返回 `HTTP 401`
- **THEN** Provider 返回 `(nil, &ProviderError{Code:"auth_failed", StatusCode:401})`；`IsRetryable()` 返回 false

### Requirement: 内部统一 Message 格式

服务 SHALL 维护内部 `Message` / `ContentBlock` 类型，语义对齐 Anthropic Messages（`role + blocks[]`，blocks 含 `text`/`tool_use`/`tool_result`）。所有 history 操作、压缩、持久化均以此格式为准。

#### Scenario: tool_use 与 tool_result 配对

- **WHEN** 一条 assistant message 含 `tool_use { id: "abc" }`
- **THEN** 紧随其后的 user message SHALL 含对应 `tool_result { tool_use_id: "abc", output }`

### Requirement: Anthropic adapter

服务 SHALL 提供 `anthropic` provider，对接 `https://api.anthropic.com/v1/messages` SSE 端点。

adapter SHALL 按以下表格把 Anthropic 8 种 SSE 事件映射为内部 ProviderEvent（5 种）：

| Anthropic SSE event | 内部 ProviderEvent | 处理说明 |
|---|---|---|
| `message_start` | （吞咽） | 仅记录 `usage.input_tokens` 到 adapter 状态，**不**透出 |
| `content_block_start` | （吞咽 / 状态） | 按 `content_block.type` 初始化累积缓冲：`text` 块准备空字符串、`tool_use` 块准备 `{id, name, input_json: ""}` |
| `content_block_delta` (type=`text_delta`) | `text_delta` | 直接透出 `text_delta` 字段 |
| `content_block_delta` (type=`input_json_delta`) | （吞咽 / 状态） | 把 `partial_json` 追加到当前 tool_use 块的 `input_json` 缓冲 |
| `content_block_delta` (type=`thinking_delta`) | （吞咽 / 状态） | MVP 不透出 thinking；累积到 thinking 缓冲，在 message_stop 时一并丢弃 |
| `content_block_stop` | `tool_use` 或（吞咽） | 当 stop 的是 tool_use 块：解析累积的 `input_json` 为 JSON，emit 完整 `tool_use { id, name, input }`；text 块：不 emit（text_delta 已实时透出） |
| `message_delta` | `usage` | 从 `delta.stop_reason` 与 `usage.output_tokens` 提取，emit `usage { input_tokens, output_tokens }` + 缓存 stop_reason 到 adapter 状态 |
| `message_stop` | `stop` | emit `stop { reason: <上一步缓存的 stop_reason> }`，然后 close channel |
| `ping` | （吞咽） | 作为底层心跳，重置 stream 读超时，不透出 |
| `error` | `error` | 翻成 ProviderError 透出，然后 close channel |

adapter SHALL：
- 直接映射内部 Message → Anthropic Messages 请求
- 在解析失败（如 SSE 帧不完整、JSON 解析失败）时 emit `error { Code: "stream_broken", Cause: err }` 后 close channel

#### Scenario: 流式接收 Anthropic SSE 简单文本

- **WHEN** Anthropic 返回 SSE 流：`message_start` → `content_block_start(text)` → `content_block_delta(text_delta)` ×5 → `content_block_stop` → `message_delta` → `message_stop`
- **THEN** adapter 透出 5 个 `text_delta`、1 个 `usage`、1 个 `stop`；`message_start`/`content_block_start`/`content_block_stop` 全部吞咽

#### Scenario: tool_use 累积完成后 emit

- **WHEN** Anthropic 返回：`content_block_start(tool_use, id=abc, name=Bash)` → `input_json_delta('{"comma')` → `input_json_delta('nd":"ls"}')` → `content_block_stop`
- **THEN** adapter 在 `content_block_stop` 时解析累积的 `{"command":"ls"}`，emit 一条 `tool_use { id:"abc", name:"Bash", input:{"command":"ls"} }`

#### Scenario: thinking 块被丢弃（MVP）

- **WHEN** Anthropic 返回 `content_block_start(thinking)` + 多个 `thinking_delta`
- **THEN** adapter 累积但不透出；MVP 不暴露 thinking 给客户端

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

#### Scenario: 并发 tool_calls 累积（index 字段）

- **WHEN** OpenAI SSE 流返回 2 个并发 tool_calls：先 `delta.tool_calls=[{index:0,id:"a",function:{name:"Bash"}}]`，然后 `delta.tool_calls=[{index:1,id:"b",function:{name:"Read"}}]`，接着交错的 arguments delta（如 `[{index:0,function:{arguments:'{"comm'}}]` → `[{index:1,function:{arguments:'{"path'}}]` → ...）
- **THEN** adapter 用 `index` 字段区分两个并发累积缓冲，最终在 `finish_reason: "tool_calls"` 时按 index 顺序 emit 2 个完整 `tool_use` 事件

#### Scenario: finish_reason="stop" 但含 tool_calls

- **WHEN** OpenAI 非流式响应返回 `finish_reason: "stop"` 但 message 含 `tool_calls`（部分模型行为）
- **THEN** adapter 按 tool_calls 数组 emit 对应 `tool_use` 事件，再 emit `stop`

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

