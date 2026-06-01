## MODIFIED Requirements

### Requirement: 内置模板

包 SHALL 导出以下预编译模板和常量：

| 名称 | 类型 | 占位符 | 用途 |
|------|------|--------|------|
| `SystemPrompt` | `*Template` | `{{.BasePrompt}}`, `{{.Instructions}}`, `{{.Environment}}`, `{{.Memory}}` | agent 系统提示词 |
| `Compaction` | `*Template` | 无（预留） | 上下文压缩摘要器提示词 |
| `SkillManifest` | `*Template` | `{{range .Skills}}` | 技能清单注入 |
| `CancelledToolOutput` | `string` | — | 取消时 tool_result 输出 |
| `CancelledNote` | `string` | — | cancelled 标记语义说明（不含前导换行） |
| `CompactionFallback` | `string` | — | 摘要失败兜底文本 |

`SystemPrompt` 模板的组装顺序 SHALL 为：`BasePrompt → CancelledNote → Environment → Instructions → Memory`，非空段之间用 `"\n\n"` 连接。`Instructions` 段位于 `Environment` 之后、`Memory` 之前。

`SystemPromptInput` struct SHALL 包含四个字段：`Base string`、`Environment string`、`Instructions string`、`Memory string`。`BuildSystemPrompt` SHALL 将所有四个字段传入模板渲染。

#### Scenario: BuildSystemPrompt 空输入

- **WHEN** `BuildSystemPrompt(SystemPromptInput{})` 被调用
- **THEN** 返回纯 `CancelledNote`，无前导空行

#### Scenario: BuildSystemPrompt 有输入

- **WHEN** `BuildSystemPrompt(SystemPromptInput{Base: "You are a helper."})` 被调用
- **THEN** 返回 `"You are a helper.\n\n" + CancelledNote`

#### Scenario: BuildSystemPrompt with instructions only

- **WHEN** `BuildSystemPrompt(SystemPromptInput{Instructions: "<instructions>...</instructions>"})` 被调用
- **THEN** 返回 `CancelledNote + "\n\n" + "<instructions>...</instructions>"`

#### Scenario: BuildSystemPrompt full assembly order

- **WHEN** `BuildSystemPrompt(SystemPromptInput{Base: "B", Environment: "E", Instructions: "I", Memory: "M"})` 被调用
- **THEN** 返回 `"B\n\n" + CancelledNote + "\n\nE\n\nI\n\nM"`
