## ADDED Requirements

### Requirement: 请求层下发 thinking 参数

启用 thinking 的会话，Anthropic adapter SHALL 在请求顶层下发 `thinking` 对象，并附带 `anthropic-beta: interleaved-thinking-2025-05-14` header。

- 配置形如 `thinking:{enabled:true, budget_tokens:N}` 时，请求体 SHALL 含 `"thinking":{"type":"enabled","budget_tokens":N}`。
- 启用 thinking 时，请求 SHALL **不**发送 `temperature` 字段（Anthropic 要求 temperature 默认为 1）。
- thinking 未启用时，请求体 SHALL 完全不含 `thinking` 字段，行为与现状一致。

#### Scenario: 启用 thinking 的请求体

- **WHEN** 会话配置 `thinking.enabled=true`、`budget_tokens=16000`
- **THEN** Anthropic 请求体含 `"thinking":{"type":"enabled","budget_tokens":16000}`，含 `anthropic-beta: interleaved-thinking-2025-05-14` header，且不含 `temperature`

#### Scenario: 未启用 thinking 的请求体

- **WHEN** 会话未配置 thinking
- **THEN** 请求体不含 `thinking` 字段，也不附带 interleaved-thinking beta header

#### Scenario: 模型不支持 thinking 时前置报错

- **WHEN** 会话配置 `thinking.enabled=true`，但所选模型不在已知支持 extended thinking 的模型集合内
- **THEN** adapter SHALL 在请求发出**之前**返回明确错误（如 `thinking not supported by model <X>`），不发出会被 Anthropic 以 400 拒绝的请求

### Requirement: 流式解析 thinking 与 signature

Anthropic adapter SHALL 解析 thinking 相关 SSE 事件并产出内部 reasoning 事件，不再丢弃：

- `content_block_start` (type=`thinking` | `redacted_thinking`)：初始化 thinking 缓冲，emit `reasoning_start`。
- `content_block_delta` (type=`thinking_delta`)：把 `thinking` 字段追加到缓冲，emit `reasoning_delta`。
- `content_block_delta` (type=`signature_delta`)：把 `signature` 累积到当前 thinking 块的 signature 缓冲，**不** emit reasoning_delta（signature 不是展示内容）。
- `content_block_stop`（thinking 块）：emit `reasoning_end`，并产出完整 thinking 块（含 text + signature）供 loop 组装进 assistant 消息。

#### Scenario: 流式接收 thinking + signature

- **WHEN** Anthropic 返回 `content_block_start(thinking)` → `thinking_delta`×3 → `signature_delta` → `content_block_stop`
- **THEN** adapter 透出 1 个 `reasoning_start`、3 个 `reasoning_delta`、1 个 `reasoning_end`；signature 累积进 thinking 块但不产生 reasoning_delta；产出的 thinking 块含累积文本与 signature

#### Scenario: redacted_thinking 块

- **WHEN** Anthropic 返回 `content_block_start(redacted_thinking)` + 数据
- **THEN** adapter 产出 `BlockRedactedThinking` 类型的块并把不透明数据保留进 `RedactedData`，emit `reasoning_start{type:"redacted"}` / `reasoning_end`（无明文 delta）

#### Scenario: 流在 signature 之前中断 → 整轮作废

- **WHEN** SSE 流在 thinking 文本之后、`signature_delta` 之前断开，导致 thinking 块缺少 signature
- **THEN** 该不完整 thinking 块 SHALL NOT 被持久化、SHALL NOT 被回传；本轮按 `stream_broken` 错误处理（沿用现有 HTTP 断线重试逻辑），重试将重新产生完整的 thinking 块

### Requirement: thinking 块的多轮回传与确定性剥离

服务 SHALL 按确定性规则把 thinking 块回传给 Anthropic。判定基准是历史中**最后一个 `end_turn` 闭合点**：

- **最后一个 `end_turn` 之后**的所有 thinking 块（即当前未闭合工具循环链条内的全部 thinking，无论该链条经过几轮 tool_use/tool_result）回传时 SHALL 原样保留其 `text` 与 `signature`（redacted 块保留 `RedactedData`）。
- **最后一个 `end_turn` 之前**（即已闭合 turn）的 thinking 块在后续请求中 SHALL 被确定性地剥离（不回传）——这些块 Anthropic 服务端本就会剥离，保留它们只会徒增上下文。
- 剥离与保留的判定 SHALL 仅依赖消息历史结构，且对同一历史多次组装 SHALL 产生逐字节一致的请求消息序列。

#### Scenario: 工具循环内回传 thinking + signature

- **WHEN** assistant 在工具循环中产出 `[thinking(sig=S), tool_use(id=abc)]`，随后 user 提供 `tool_result(abc)`，需再次调用 LLM
- **THEN** 回传请求中保留该 thinking 块及其 `signature=S`

#### Scenario: 同一活跃工具循环内多个 thinking 块全部保留

- **WHEN** 自最后一个 `end_turn` 起，活跃链条为 `thinking_A → tool_use(Read) → tool_result → thinking_B → tool_use(Write) → tool_result`，需再次调用 LLM（尚未出现新的 `end_turn`）
- **THEN** 回传请求中 `thinking_A` 与 `thinking_B` 连同各自 signature **全部保留**（不因 thinking_A 较早而被剥离）

#### Scenario: turn 闭合后剥离历史 thinking

- **WHEN** 一个 turn 以 `end_turn` 闭合，进入下一轮用户输入
- **THEN** 下一轮请求中该 turn 内的所有 thinking 块（含其活跃链条内的 thinking_A/thinking_B）被剥离；对同一历史重复组装两次得到逐字节相同的消息序列

### Requirement: thinking 块的持久化

服务 SHALL 把 thinking / redacted_thinking 块（thinking 含 `signature`，redacted 含 `RedactedData`）写入 `messages.ContentJSON`，与 text/tool_use 块并存于同一条 assistant 消息，顺序保持产出顺序。thinking 正文 SHALL NOT 进入 FTS5 `messages_fts` 索引（推理过程是中间产物，避免污染 session_search 结果）。

#### Scenario: thinking 块持久化并可重建

- **WHEN** 一条 assistant 消息含 `[thinking(sig=S), text, tool_use]`
- **THEN** 持久化后从 `messages.ContentJSON` 读回该消息，thinking 块的文本与 `signature=S` 完整保留，块顺序不变

#### Scenario: redacted_thinking 持久化往返无损

- **WHEN** 一条 assistant 消息含 `redacted_thinking { RedactedData: D }`
- **THEN** 持久化并读回后 `RedactedData=D` 字节不变，可原样回传 Anthropic
