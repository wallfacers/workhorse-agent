## MODIFIED Requirements

### Requirement: 内部统一 Message 格式

服务 SHALL 维护内部 `Message` / `ContentBlock` 类型，语义对齐 Anthropic Messages（`role + blocks[]`，blocks 含 `text`/`tool_use`/`tool_result`/`thinking`/`redacted_thinking`）。所有 history 操作、压缩、持久化均以此格式为准。

`ContentBlock` SHALL 为 `thinking` / `redacted_thinking` 块新增字段：

- `Thinking string`：thinking 块的明文推理内容（仅 `BlockThinking` 使用）。
- `Signature string`：thinking 块的密码学签名，回传 Anthropic 时原样携带（仅 `BlockThinking` 使用）。
- `RedactedData string`：`redacted_thinking` 块的不透明加密内容（Anthropic 块的 `data` 字段，base64），回传时原样携带（仅 `BlockRedactedThinking` 使用）。

`BlockType` SHALL 新增 `BlockThinking` 与 `BlockRedactedThinking`。

#### Scenario: tool_use 与 tool_result 配对

- **WHEN** 一条 assistant message 含 `tool_use { id: "abc" }`
- **THEN** 紧随其后的 user message SHALL 含对应 `tool_result { tool_use_id: "abc", output }`

#### Scenario: thinking 块携带 signature

- **WHEN** 一条 assistant message 含 `thinking { text, signature }`
- **THEN** 该块以 `BlockThinking` 类型表示，`Thinking` 与 `Signature` 字段均被保留，回传时原样携带

#### Scenario: redacted_thinking 块保留不透明数据

- **WHEN** Anthropic 返回 `redacted_thinking { data: <base64> }`
- **THEN** 该块以 `BlockRedactedThinking` 类型表示，`RedactedData` 保留原始 `data` 字节，回传时原样携带

### Requirement: Anthropic adapter

服务 SHALL 提供 `anthropic` provider，对接 `https://api.anthropic.com/v1/messages` SSE 端点。

adapter SHALL 按以下表格把 Anthropic SSE 事件映射为内部 ProviderEvent：

| Anthropic SSE event | 内部 ProviderEvent | 处理说明 |
|---|---|---|
| `message_start` | （吞咽） | 仅记录 `usage.input_tokens` 到 adapter 状态，**不**透出 |
| `content_block_start` (text/tool_use) | （吞咽 / 状态） | 按 `content_block.type` 初始化累积缓冲 |
| `content_block_start` (thinking/redacted_thinking) | `reasoning_start` | 初始化 thinking 缓冲（text + signature），emit `reasoning_start` |
| `content_block_delta` (type=`text_delta`) | `text_delta` | 直接透出 `text_delta` 字段 |
| `content_block_delta` (type=`input_json_delta`) | （吞咽 / 状态） | 把 `partial_json` 追加到当前 tool_use 块的 `input_json` 缓冲 |
| `content_block_delta` (type=`thinking_delta`) | `reasoning_delta` | 把 `thinking` 追加到 thinking 缓冲并透出 `reasoning_delta` |
| `content_block_delta` (type=`signature_delta`) | （吞咽 / 状态） | 把 `signature` 累积到当前 thinking 块，**不**透出 reasoning_delta |
| `content_block_stop` (tool_use) | `tool_use` | 解析累积 `input_json`，emit 完整 `tool_use { id, name, input }` |
| `content_block_stop` (thinking) | `reasoning_end` | emit `reasoning_end`，产出完整 thinking 块（text + signature）供 loop 组装 |
| `content_block_stop` (text) | （吞咽） | text_delta 已实时透出，不再 emit |
| `message_delta` | `usage` | 提取 `delta.stop_reason` 与 `usage.output_tokens`，emit `usage` + 缓存 stop_reason |
| `message_stop` | `stop` | emit `stop { reason }`，然后 close channel |
| `ping` | （吞咽） | 心跳，重置读超时，不透出 |
| `error` | `error` | 翻成 ProviderError 透出，然后 close channel |

adapter SHALL：
- 把内部 Message → Anthropic Messages 请求，`system` 字段序列化为带 `cache_control` 的 block 数组（见 prompt-cache 能力）
- 启用 thinking 时下发顶层 `thinking` 参数与 interleaved-thinking beta header（见 extended-thinking 能力）
- 回传 thinking 块时按"未闭合工具循环内保留、闭合后剥离"的确定性规则处理（见 extended-thinking 能力）
- 在解析失败时 emit `error { Code: "stream_broken", Cause: err }` 后 close channel

#### Scenario: 流式接收 Anthropic SSE 简单文本

- **WHEN** Anthropic 返回 SSE 流：`message_start` → `content_block_start(text)` → `content_block_delta(text_delta)` ×5 → `content_block_stop` → `message_delta` → `message_stop`
- **THEN** adapter 透出 5 个 `text_delta`、1 个 `usage`、1 个 `stop`；`message_start`/`content_block_start`/`content_block_stop` 全部吞咽

#### Scenario: tool_use 累积完成后 emit

- **WHEN** Anthropic 返回：`content_block_start(tool_use, id=abc, name=Bash)` → `input_json_delta('{"comma')` → `input_json_delta('nd":"ls"}')` → `content_block_stop`
- **THEN** adapter 在 `content_block_stop` 时解析累积的 `{"command":"ls"}`，emit 一条 `tool_use { id:"abc", name:"Bash", input:{"command":"ls"} }`

#### Scenario: thinking 块被解析并透出

- **WHEN** Anthropic 返回 `content_block_start(thinking)` → `thinking_delta`×3 → `signature_delta` → `content_block_stop`
- **THEN** adapter 透出 1 个 `reasoning_start`、3 个 `reasoning_delta`、1 个 `reasoning_end`；signature 累积进 thinking 块但不产生 reasoning_delta；产出的 thinking 块含累积文本与 signature

### Requirement: Provider 接口

服务 SHALL 定义统一 Provider 接口：

```go
type Provider interface {
    Name() string
    Stream(ctx context.Context, req Request) (<-chan ProviderEvent, error)
}
```

`Request` SHALL 携带 thinking 配置（启用标志与 budget_tokens），adapter 据此决定是否下发顶层 `thinking` 参数。

内部 `ProviderEvent` 的 `EventType` SHALL 新增 reasoning 三段事件：`reasoning_start`、`reasoning_delta`、`reasoning_end`，分别携带 thinking 块的开始信号、增量明文、结束信号（reasoning_end 附带完整 thinking 块供组装）。

`Stream` 的返回值语义 SHALL 严格区分：

- 返回的 **`error` ≠ nil**：表示"请求**未发出**"或"HTTP 请求即失败"。此时 channel 为 nil 或已关闭。
- 返回的 **error == nil**：表示"请求已发出且 HTTP 200，正在 stream"。后续错误通过 channel 上的 `error` 事件传递。

channel 在以下任一情况后 SHALL close：(a) 收到 `stop` 事件、(b) ctx 被 cancel、(c) HTTP 连接断开。close 前 SHALL emit 一条对应的 `stop` 或 `error` 事件作为流终点。

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

#### Scenario: reasoning 事件透出

- **WHEN** Anthropic 流含 thinking 块
- **THEN** channel 依次透出 `reasoning_start`、若干 `reasoning_delta`、`reasoning_end`；`reasoning_end` 携带完整 thinking 块（text + signature）
