## 1. Provider 类型扩展（向后兼容加字段）

- [x] 1.1 `internal/provider/provider.go`：`BlockType` 新增 `BlockThinking`、`BlockRedactedThinking`
- [x] 1.2 `provider.go`：`ContentBlock` 新增 `Thinking string`、`Signature string`（BlockThinking 用）、`RedactedData string`（BlockRedactedThinking 用，承载 Anthropic `data` 不透明字节）字段，补注释说明各自适用块类型
- [x] 1.3 `provider.go`：`Request` 新增 thinking 配置字段（`ThinkingEnabled bool`、`ThinkingBudgetTokens int`）
- [x] 1.4 `provider.go`：`EventType` 新增 `EventReasoningStart`、`EventReasoningDelta`、`EventReasoningEnd`；`ProviderEvent` 新增承载字段（reasoning 文本增量、block_index、reasoning_end 携带完整 thinking 块）

## 2. Anthropic adapter：解析与请求

- [x] 2.1 `anthropic/wire.go`：`anthropicReq.System` 由 `string` 改为 system block 数组类型；新增顶层 `Thinking` 请求字段类型
- [x] 2.2 `anthropic/wire.go`：sse delta 类型补 `signature_delta` 的 `signature` 字段；content_block_start 补 thinking/redacted_thinking 的块数据字段
- [x] 2.3 `anthropic/anthropic.go` `encodeRequest`：System 序列化为 `[{type:text,text,cache_control:{type:ephemeral}}]`；空 system 不发
- [x] 2.4 `encodeRequest`：`ThinkingEnabled` 时下发 `thinking:{type:enabled,budget_tokens}`，省略 `temperature`，附 `anthropic-beta: interleaved-thinking-2025-05-14` header（header 值定义为常量，旁注 `// TODO: remove when promoted from beta`）
- [x] 2.4a 模型支持前置门控：维护"已知支持 extended thinking 的 Anthropic 模型集合"，`ThinkingEnabled` 但模型不在集合内时在请求发出前返回明确错误（不发请求）
- [x] 2.5 `anthropic/stream_state.go`：`blockBuf` 增 thinking 文本与 signature 缓冲；content_block_start(thinking/redacted_thinking) → 初始化缓冲 + emit `reasoning_start`
- [x] 2.6 `stream_state.go`：`thinking_delta` → 累积 + emit `reasoning_delta`（替换原 return nil 丢弃）；`signature_delta` → 仅累积，不 emit
- [x] 2.7 `stream_state.go`：content_block_stop(thinking) → emit `reasoning_end` 携带完整 thinking 块（text+signature）
- [x] 2.7a `stream_state.go`：流在 signature 之前中断导致 thinking 块无 signature 时，不产出该块，本轮按 `stream_broken` 错误处理（沿用现有断线逻辑）
- [x] 2.8 `anthropic.go` 回传组装（toAnthropicMessage）：序列化 thinking 块（含 signature）与 redacted_thinking 块（含 RedactedData）

## 3. thinking 回传确定性规则

- [x] 3.1 实现"未闭合工具循环内保留、end_turn 闭合后剥离"的历史扫描逻辑（确定性，仅依赖历史结构）
- [x] 3.2 确保 cache_control 断点不挂在 thinking 块上（仅 system 末尾单断点）

## 4. Agent loop

- [x] 4.1 `internal/agent/loop.go` `consumeProviderStream`：新增 reasoning 三事件 case，累积 thinking 块
- [x] 4.2 `consumeProviderStream`：将 thinking 块（含 signature）按产出顺序组装进 assistant 消息，与 text/tool_use 并存
- [x] 4.3 `consumeProviderStream`：reasoning 事件实时 emit SSE，确保 signature 不进 emit payload

## 5. SSE 协议

- [x] 5.1 `internal/api/protocol/protocol.go`：新增 `reasoning_start`、`reasoning_delta`、`reasoning_end` ServerEventType（核心事件 11→14）
- [x] 5.2 定义三事件的 JSON payload 形状：`reasoning_start{block_index, type:"thinking"|"redacted"}`、`reasoning_delta{block_index, delta}`、`reasoning_end{block_index}`
- [x] 5.3 `docs/protocol.md`：更新 Server→Client 事件表，补 reasoning 事件说明、`type` 区分 thinking/redacted、signature 与 redacted data 不暴露的契约

## 6. 配置

- [x] 6.1 `internal/config/config.go`：`AgentConfig` 增 `thinking{enabled,budget_tokens}`
- [x] 6.2 `internal/config/validate.go`：`enabled=true` 时校验 `budget_tokens>0` 且 `budget_tokens<=max_tokens`
- [x] 6.3 会话创建处：thinking 配置随 session 冻结，无运行时调参接口；非 Anthropic provider 忽略 thinking 配置
- [x] 6.4 将冻结的 thinking 配置接入 `provider.Request` 构建

## 7. 持久化

- [x] 7.1 确认 thinking/redacted_thinking 块（含 signature）经 `messages.ContentJSON` 往返无损（无 schema 变更）
- [x] 7.2 确认投影给客户端的消息历史中 signature 被剥离

## 8. 测试

- [x] 8.1 改写 `TestAnthropic_ThinkingDiscarded` → 断言 thinking 被解析为 reasoning 事件 + 产出 thinking 块（含 signature）
- [x] 8.2 新增：启用/未启用 thinking 的请求体断言（thinking 字段、beta header、无 temperature）；模型不支持 thinking 时前置报错
- [x] 8.3 新增：System block 数组 + cache_control 序列化断言；空 system 不发
- [x] 8.4 新增 byte-stable 测试：同历史两次组装请求消息序列逐字节相同
- [x] 8.5 新增 byte-stable 测试：同会话跨 turn 顶层 thinking 参数逐字节相同
- [x] 8.6 新增：thinking 回传规则——活跃工具循环内多个 thinking 块全部保留（thinking_A/thinking_B）、end_turn 后全部剥离
- [x] 8.7 新增：reasoning SSE 事件序列断言；`reasoning_start.type` 区分 thinking/redacted；signature 与 redacted data 不出现在任何 SSE payload
- [x] 8.8 新增：thinking + redacted_thinking 块 ContentJSON 往返无损（含 RedactedData）；config thinking 校验（budget=0、budget>max_tokens 报错）
- [x] 8.9 新增：流在 signature 前中断 → thinking 块不持久化/不回传、按 stream_broken 处理
- [x] 8.10 更新受 system 数组化影响的硬编码 baseline 测试（按 adapter 测试文件分批，逐文件重采）
- [x] 8.11 `golangci-lint run` 干净

## 9. 集成与收尾

- [x] 9.1 端到端集成测试：启用 thinking 的会话完成一轮工具调用，验证（a）SSE 事件序列正确（含 reasoning 三段）（b）thinking 块持久化（c）重放历史后请求前缀逐字节稳定
- [x] 9.2 更新 `CLAUDE.md` Memory/Provider 相关段落，记录 thinking 支持与显式 cache_control 已激活
- [x] 9.3 全量 `go test ./...` 通过
