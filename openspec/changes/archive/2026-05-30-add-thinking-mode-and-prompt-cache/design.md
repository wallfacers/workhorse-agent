## Context

Anthropic extended thinking 当前在 `internal/provider/anthropic/stream_state.go:97-99` 被故意丢弃，`TestAnthropic_ThinkingDiscarded` 锁死了该行为。与此同时，仓库虽做过两轮"前缀字节稳定性"优化（归档的 `optimize-prompt-cache-order`、`base-first` 提交），但**显式 prompt cache 实际从未激活**：全仓库无 `cache_control`，且 `anthropicReq.System` 是纯字符串（`wire.go:12`），结构上无法挂断点。

这两件事强耦合：thinking 块进入消息历史会改变缓存前缀的字节序列。若分两次做，先做的 thinking wire 格式会被后做的 cache_control 改造推翻。因此合并设计、一次到位。

参考实现来自 explore 阶段对 `../claude-code-sourcemap`（贴 Anthropic 原生 SSE + signature 回传）与 `../opencode`（provider 无关的 reasoning-start/delta/end 三段抽象 + ReasoningPart）的研究。

## Goals / Non-Goals

**Goals:**
- Anthropic provider 支持 extended thinking：请求下发 `thinking` 参数、流式解析 thinking/signature、持久化、确定性多轮回传。
- thinking 正文实时流式推给客户端（reasoning 三段事件），signature 不入 SSE。
- 落地真正的显式 prompt cache：System block 数组化 + `cache_control:ephemeral` 断点。
- 用 byte-stable 测试守护两个不变量：缓存前缀稳定性、thinking 回传确定性。

**Non-Goals:**
- OpenAI provider 的 reasoning effort / encrypted reasoning（留作后续）。
- 多断点缓存策略（tools、长 history 的多级 cache_control）——本次只激活 system 前缀单断点，message 级断点按 thinking 回传规则自然落位即可。
- 客户端 UI 的具体展示（折叠/高亮等由客户端自行决定）。

## Decisions

### D1：内部事件用 provider 无关的 reasoning 三段式，而非贴 Anthropic 原生

新增 `EventReasoningStart` / `EventReasoningDelta` / `EventReasoningEnd`，而不是直接透出 `thinking_delta` / `signature_delta`。

- **理由**：契合既有的 Anthropic "8→5 折叠"抽象（adapter 把 vendor 事件折叠成内部事件），与 opencode 的 ReasoningPart 同构，未来接 OpenAI/Gemini reasoning 时 loop 层无需改动。
- **signature 不进事件流**：signature 在 adapter 内部累积进 thinking 块，`reasoning_end` 携带完整块（含 signature）供 loop 组装。客户端 SSE 永远看不到 signature。
- **备选**：直接透出 Anthropic 原生 delta —— 否决，会把 vendor 细节泄漏到 loop 与 SSE 协议，且 signature 难以干净隔离。

### D2：System 改为 `[]contentBlock`，单断点挂末尾

`anthropicReq.System` 从 `string` 改为 `json.RawMessage` 或 `[]systemBlock`，序列化为 `[{"type":"text","text":T,"cache_control":{"type":"ephemeral"}}]`。

- **理由**：cache_control 必须挂在 block 上，string 形态无法激活缓存。System 是最稳定的前缀（base-first 已保证），单断点即可覆盖跨会话/跨 turn 的最大稳定前缀。
- **空 system 不发**：保持与现状一致，避免空 block。
- **测试影响**：所有硬编码 system 期望值的测试需重采 baseline（string → 数组）。

### D3：thinking 配置随 session 冻结，复用 memory snapshot 模式

`AgentConfig.thinking{enabled,budget_tokens}` 在会话创建时读取并冻结，无运行时调参接口。

- **理由**：改 thinking 开关/budget 会使 Anthropic 消息缓存前缀失效。冻结是保证"同会话跨 turn 顶层 thinking 参数逐字节一致"的最简手段，与现有 memory snapshot 的"启动即冻结"一致。
- **备选**：允许运行时调 budget —— 否决，等于每次调参都炸缓存，且与"缓存别失效"的核心约束冲突。

### D4：thinking 回传遵循"未闭合工具循环内保留、闭合后剥离"，且确定性

回传组装（`toAnthropicMessage` / 请求构建处）的规则：

```
对历史从后往前扫描：
  找到最后一个 end_turn 闭合点
  闭合点之后（含正在进行的工具循环）的 thinking 块 → 保留 text + signature 回传
  闭合点之前的 thinking 块 → 剥离（不回传）
```

- **理由**：Anthropic 要求工具循环内的 thinking 块连 signature 原样回传（否则 400），但已闭合 turn 的 thinking 不需要回传。规则只依赖历史结构，对同一历史多次组装产出逐字节一致的消息序列 —— 这是缓存命中的前提。
- **断点位置**：cache_control 永不挂 thinking 块；若需 message 级断点，落在其后的 tool_result/text。本次 system 单断点已足够，message 级断点暂不引入（Non-Goal）。

### D5：持久化复用 ContentJSON，无 schema 变更

thinking/redacted_thinking 块作为 `ContentBlock` 直接进 `messages.ContentJSON` 的 `[]ContentBlock` 序列化，无需改 `messages` 表结构。thinking 块带 `Thinking`+`Signature`，redacted 块带 `RedactedData`（Anthropic `data` 字段，base64 不透明字节，回传时原样携带）。

- **理由**：ContentBlock 是 JSON 序列化，加字段即向后兼容。
- **FTS5（已决定）**：thinking 正文**不进** FTS5 `messages_fts` 索引。推理是内部中间产物，进 FTS 只会污染 session_search 结果。

### D6：thinking 模型支持前置门控

adapter 维护一个"已知支持 extended thinking 的 Anthropic 模型集合"，`enabled=true` 但模型不在集合内时**在请求发出前**返回明确错误，而非发出请求等 Anthropic 400。

- **理由**：快速失败、错误信息可读；避免一次往返才发现配置无效。
- **temperature 非冲突项**：经核实 `Temperature` 仅存在于 `provider.Request`，`AgentConfig` 无 temperature 配置项、当前恒为 0 从不下发，故"启用 thinking 静默丢弃用户 temperature"的冲突当前不存在。待 temperature 成为可配项时再补"thinking 启用时拒绝显式 temperature"的校验（YAGNI，本次不做）。

## Risks / Trade-offs

- **[缓存前缀静默失效]** thinking 回传不确定性或 thinking 参数漂移会让缓存 miss，且无报错、只表现为成本上升 → 用 byte-stable 测试锁定"同历史两次组装逐字节相同""同会话跨 turn thinking 参数相同"两个不变量。
- **[System 数组化破坏大量测试 baseline]** → 一次性重采，集中在一个提交；boundary 测试确认 prompt 包仍 IO-free。
- **[signature 泄漏到客户端]** 是隐私/安全问题 → 在 SSE 层与持久化-给客户端的投影处显式断言 signature 字段被剥离，加测试覆盖。
- **[redacted_thinking 处理不当导致 400]** redacted 块带不透明数据，必须原样回传 → 当作黑盒字节保留，回传规则与普通 thinking 一致。
- **[interleaved-thinking beta header 失效/变更]** Anthropic beta 标识可能演进 → header 值集中为常量，便于单点更新；drift 通过请求失败可见。

## Migration Plan

1. 先落 `provider.go` 类型扩展（BlockType/ContentBlock/EventType）—— 纯加字段，向后兼容。
2. 落 wire/adapter 解析与 System 数组化 + cache_control；改写 `TestAnthropic_ThinkingDiscarded`，重采 system baseline。
3. 落 loop 组装与回传确定性规则 + byte-stable 测试。
4. 落 SSE 协议事件 + docs/protocol.md。
5. 落 config thinking 冻结 + 校验。
6. **回滚**：thinking 默认 `enabled:false`，未开启时行为与现状等价（thinking 不发、解析路径不触发）；System 数组化是无条件改动但语义等价（内容字节不变），风险隔离在测试 baseline。

## Open Questions

- **budget_tokens 模型感知上限**：不同 Anthropic 模型对 thinking 预算有不同上限。本次仅做"budget ≤ max_tokens"的基础校验（见 configuration spec）；是否引入模型→上限的 map 做精确校验待定，倾向延后到模型能力矩阵稳定后再加，避免硬编码易过时的常量表。
- **adaptive thinking**：本次只做 `enabled+budget_tokens`。将来支持 `thinking:{type:"adaptive"}` 时，倾向新增显式 `mode` 字段（`"enabled"|"adaptive"`）而非用 `budget_tokens=0` 重载语义——保持 `budget_tokens>0` 校验干净。
- 客户端是否需要 `assistant_text_done` 之外的 thinking 完成聚合信号？当前 `reasoning_end` 已足够，暂不加聚合事件。
