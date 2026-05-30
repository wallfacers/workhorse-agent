## ADDED Requirements

### Requirement: System 字段 block 数组化并挂 cache_control

Anthropic adapter SHALL 把请求的 `system` 字段从纯字符串改为 content block 数组，并在末尾块挂 `cache_control:{type:"ephemeral"}` 以激活前缀缓存。

- `system` SHALL 序列化为 `[{"type":"text","text":<system prompt>,"cache_control":{"type":"ephemeral"}}]`。
- 当 system prompt 为空时，SHALL 不发送 `system` 字段（与现状一致），不产生空 block。
- 该转换 SHALL 不改变 system prompt 的文本内容（与 base-first 组装顺序产出的字符串逐字节一致）。

#### Scenario: system 序列化为带 cache_control 的 block 数组

- **WHEN** 组装出的 system prompt 文本为 T
- **THEN** Anthropic 请求体 `system` 为 `[{"type":"text","text":T,"cache_control":{"type":"ephemeral"}}]`

#### Scenario: 空 system 不发送

- **WHEN** system prompt 为空字符串
- **THEN** 请求体不含 `system` 字段

### Requirement: 缓存断点位置避开 thinking 块

服务设置的任何 `cache_control` 断点 SHALL 落在稳定内容块（system 末尾、tool_result 或 text 块）上，SHALL NOT 挂在 thinking / redacted_thinking 块上。

#### Scenario: 断点不落在 thinking 块

- **WHEN** 一条 assistant 消息以 thinking 块结尾
- **THEN** 该消息不在 thinking 块上设置 cache_control；若需在该消息设断点，落在其后的稳定块上

### Requirement: thinking 配置变更与缓存前缀一致性

服务 SHALL 保证 thinking 配置在单个会话生命周期内不可变（见 configuration 能力）。由此，同一会话内跨 turn 的请求 SHALL 保持顶层 `thinking` 参数逐字节一致，使消息缓存前缀不因 thinking 参数变化而失效。

#### Scenario: 同会话跨 turn thinking 参数稳定

- **WHEN** 同一会话连续两轮请求
- **THEN** 两次请求的顶层 `thinking` 对象逐字节相同；缓存前缀不因 thinking 参数变化而失效
