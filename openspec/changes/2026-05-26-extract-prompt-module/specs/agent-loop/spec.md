## ADDED Requirements

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
