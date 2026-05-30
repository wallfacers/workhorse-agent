## ADDED Requirements

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
