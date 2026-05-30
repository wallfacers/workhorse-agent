## Why

Anthropic 的 extended thinking（扩展思考）目前在 provider adapter 最底层被**故意丢弃**（`stream_state.go:97-99` 吞掉 `thinking_delta`，并有 `TestAnthropic_ThinkingDiscarded` 锁死该行为），客户端因此完全看不到模型的推理过程，多工具轮次也无法回传 thinking 块。与此同时，仓库里其实**还没有真正激活显式 prompt cache**：全仓库无 `cache_control`，`anthropicReq.System` 是纯字符串（`wire.go:12`），挂不上断点；已归档的 `optimize-prompt-cache-order` 只是保证了"前缀字节稳定性"的铺垫。这两件事强耦合——thinking 块进入消息历史会改变缓存前缀的字节序列，缓存断点必须避开 thinking 块、System 字段要从 string 改成 block 数组、thinking 回传策略必须确定性，否则任何一处不一致都会让缓存 miss。分两次做会让第二次推翻第一次的 wire 格式，故合并设计、一次到位。

## What Changes

- **新增 extended thinking 支持（仅 Anthropic provider）**：请求顶层下发 `thinking:{type,budget_tokens}`，附带 `interleaved-thinking-2025-05-14` beta header；解析 `content_block_start{thinking}` / `thinking_delta` / `signature_delta` / `content_block_stop`，不再吞掉。
- **新增 thinking / redacted_thinking 内容块**：`provider.BlockType` 增 `BlockThinking`、`BlockRedactedThinking`；`ContentBlock` 增 `Thinking`、`Signature`（thinking 用）、`RedactedData`（redacted 用，承载 Anthropic `data` 不透明字节）字段。
- **新增 reasoning 内部事件三段式**：`reasoning_start` / `reasoning_delta` / `reasoning_end`（对标 opencode `ReasoningPart` 抽象，契合现有 8→5 provider 折叠）。
- **新增客户端 SSE 事件**：`reasoning_start` / `reasoning_delta` / `reasoning_end` 实时流式推送 thinking 正文，客户端自行决定展示（默认折叠）。`signature` **不进 SSE**，仅服务端持久化与回传。
- **持久化 thinking 块**：thinking / redacted_thinking 块（含 `signature`）写入 `messages.ContentJSON`，并按"只回传未闭合工具循环内 thinking"的确定性规则回传给 API。
- **落地显式 prompt cache**：`anthropicReq.System` 从 `string` 改为 content block 数组，末尾块挂 `cache_control:{type:"ephemeral"}`，真正激活前缀缓存。缓存断点位置确定性、避开 thinking 块。
- **新增 thinking 配置**：`AgentConfig` 增 thinking 配置（`enabled` + `budget_tokens`），随 session 启动**冻结不可变**（复用 memory snapshot 模式），禁止中途调参以防缓存前缀失效。
- 修改 `TestAnthropic_ThinkingDiscarded` 语义（从"丢弃"改为"解析并产出 thinking 块"）。
- **范围外**：OpenAI provider 的 reasoning effort 留作后续；本次仅 Anthropic 启用 thinking 与 cache_control。

## Capabilities

### New Capabilities
- `extended-thinking`: Anthropic 扩展思考的请求参数、流式解析、thinking/redacted_thinking 内容块、signature 处理、多轮回传与确定性剥离规则。
- `prompt-cache`: 显式 `cache_control` 断点的下发、System block 数组化、断点位置确定性规则（避开 thinking 块）、与 thinking 配置冻结的协同。

### Modified Capabilities
- `provider-abstraction`: `BlockType` 新增 thinking/redacted_thinking；`ContentBlock` 新增 Thinking/Signature 字段；`Request` 新增 thinking 配置；`EventType` 新增 reasoning 三段事件。
- `agent-loop`: `consumeProviderStream` 处理 reasoning 事件、累积 thinking、将 thinking 块组装进 assistant 消息并 emit SSE。
- `api-protocol`: 新增 `reasoning_start` / `reasoning_delta` / `reasoning_end` ServerEvent。
- `configuration`: `AgentConfig` 新增 session 内不可变的 thinking 配置。

## Impact

- **代码**：`internal/provider/anthropic/{wire.go,anthropic.go,stream_state.go}`、`internal/provider/provider.go`、`internal/agent/loop.go`、`internal/api/protocol/protocol.go`、`internal/config/config.go`、`internal/store`（无 schema 变更，ContentBlock JSON 扩展即可）、`docs/protocol.md`。
- **测试**：改写 `TestAnthropic_ThinkingDiscarded`；新增缓存前缀字节稳定性测试、thinking 多轮回传/剥离确定性测试、System block 数组序列化测试。
- **行为**：Anthropic 请求体 `system` 字段由 string 变为 block 数组（所有硬编码 system 期望值的测试需重采 baseline）；启用 thinking 时不发 `temperature`（Anthropic 要求 temp=1）。
- **依赖**：无新增 go.mod 依赖。
- **风险**：中。主要在缓存前缀稳定性与 thinking 回传确定性两个不变量——任一处不一致即静默掉缓存命中率，需 byte-stable 测试守护。
