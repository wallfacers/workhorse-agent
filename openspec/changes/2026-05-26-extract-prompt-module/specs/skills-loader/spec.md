## ADDED Requirements

### Requirement: 技能清单模板由 prompt 包管理

`FormatManifest` SHALL 内部调用 `internal/prompt` 包的 `SkillManifest.Execute`
渲染技能清单，而非自行用 `strings.Builder` 手拼。`prompt` 包提供：

- `SkillManifest` 模板 — 带 `{{range .Skills}}` 占位符，footer 写死为英文

`FormatManifest` 签名不变（接收 `*Catalog`，返回 `string`），调用方无需感知模板层的存在。
触发器空白折叠（`strings.Fields`）SHALL 在 `FormatManifest` 中完成，不在模板层做。

#### Scenario: FormatManifest 输出等价

- **WHEN** `FormatManifest(cat)` 被调用，`cat` 含两个 skill
- **THEN** 输出含 `<available_skills>` 包裹的 name + trigger 列表 + 英文 footer，
  与迁移前行为一致（footer 语言从中文改为英文除外）

- **WHEN** `FormatManifest(nil)` 被调用
- **THEN** 返回空字符串

## MODIFIED Requirements

### Requirement: System prompt 中暴露 skill 清单

在 `cmd_serve.go` 的 `newRunnerFactory` 中，设置 `loop.SystemPromptBase` 后，
SHALL 调用 `skills.FormatManifest(skillCatalog)` 并将非空结果追加到
`SystemPromptBase`。追加时 SHALL 正确处理空 base：若 `SystemPromptBase` 为空，
manifest 直接赋值，不产生前导换行。

本接线只覆盖 `newRunnerFactory` 创建的顶层会话。`dispatch.go` 派生的子 agent
不接 skill manifest——子 agent 的 `allowed_tools` 通常由 agent 定义窄化，
是否需要 `LoadSkill` 由 agent 定义控制。

#### Scenario: Skill manifest 出现在 system prompt 中

- **WHEN** `skillCatalog` 含至少一个 skill，且会话被创建（无 `system_prompt_base`）
- **THEN** 该会话的 system prompt 包含 `<available_skills>` 块，无前导空行

- **WHEN** `skillCatalog` 含至少一个 skill，且会话有非空 `system_prompt_base`
- **THEN** 该会话的 system prompt 在 base 末尾追加 `\n\n` + manifest 块
