# skills-loader Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: Skill 配置发现

服务 SHALL 在每次创建会话时扫描 `~/.workhorse-agent/skills/*/skill.yaml`，加载所有 skill 定义。

**已知限制（V2 增强）**：会话运行中新增的 skill 对已存在的 session **不可见**；用户需销毁重建 session 才能看到。V2 计划加文件监听 + 手动 `POST /v1/sessions/{id}/refresh-skills` API。

skill.yaml 格式：

```yaml
name: <kebab-case 名称>
description: <短描述>
trigger: |
  <自然语言条件，说明何时该用这个 skill>
content_path: ./instructions.md   # 相对于 skill.yaml
allowed_tools:                    # 可选，限制 LoadSkill 后会话可调的工具子集
  - Read
  - Bash
```

#### Scenario: 多 skill 加载

- **WHEN** `~/.workhorse-agent/skills/` 下有 `git-helper/skill.yaml` 和 `code-review/skill.yaml`
- **THEN** 创建会话时两个 skill 都被加载

#### Scenario: 缺少 content_path 文件

- **WHEN** `skill.yaml` 引用的 `content_path` 文件不存在
- **THEN** 该 skill 跳过加载，日志 emit `warn { skill: <name>, reason: "content_path not found" }`，其他 skill 继续加载

### Requirement: System prompt 中暴露 skill 清单

服务 SHALL 在 `cmd_serve.go` 的 `newRunnerFactory` 中，设置 `loop.SystemPromptBase` 后
调用 `skills.FormatManifest(skillCatalog)` 并将非空结果追加到 `SystemPromptBase`。
追加 SHALL 正确处理空 base：若 `SystemPromptBase` 为空，
manifest 直接赋值，不产生前导换行。

本接线只覆盖 `newRunnerFactory` 创建的顶层会话。`dispatch.go` 派生的子 agent
不接 skill manifest——子 agent 的 `allowed_tools` 通常由 agent 定义窄化，
是否需要 `LoadSkill` 由 agent 定义控制。

manifest 输出格式由 `internal/prompt.SkillManifest` 模板决定（见 prompt-module spec），
footer 为英文 `"You can use the LoadSkill tool to load full instructions."`。

#### Scenario: Skill manifest 出现在 system prompt 中

- **WHEN** `skillCatalog` 含至少一个 skill，且会话被创建（无 `system_prompt_base`）
- **THEN** 该会话的 system prompt 包含 `<available_skills>` 块，无前导空行

- **WHEN** `skillCatalog` 含至少一个 skill，且会话有非空 `system_prompt_base`
- **THEN** 该会话的 system prompt 在 base 末尾追加 `\n\n` + manifest 块

#### Scenario: 无 skill 时不追加

- **WHEN** `skillCatalog` 为空（`FormatManifest` 返回 `""`）
- **THEN** system prompt 不含 `<available_skills>` 块

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

### Requirement: LoadSkill 内置工具

服务 SHALL 内置 `LoadSkill` 工具，签名：

```go
type LoadSkillInput struct {
    Name string `json:"name"`  // skill name，必需
}
```

行为：
- 读取该 skill 的 `content_path` 内容
- 返回 `tool_result { output: <文件内容> }`，由 LLM 在下一轮看到并参与推理
- 若 skill 定义有 `allowed_tools`，会话的可用工具集 SHALL 在本次 LoadSkill 调用后被收窄到该子集（直到会话结束或被另一个 LoadSkill 覆盖）

`LoadSkill.IsReadOnly = true`，`CanRunInParallel = true`（多个 skill 可同时加载）。

#### Scenario: 加载并使用 skill

- **WHEN** LLM 调 `LoadSkill { name: "git-helper" }`
- **THEN** 返回 `tool_result` 含 `instructions.md` 完整内容；LLM 下一轮基于此内容继续推理

#### Scenario: 加载不存在的 skill

- **WHEN** LLM 调 `LoadSkill { name: "nonexistent" }`
- **THEN** 返回 `tool_result { is_error: true, output: "skill not found: nonexistent" }`

### Requirement: 同名 skill 冲突

不同目录但同名（`name` 字段相同）的 skill SHALL 视为冲突：服务 SHALL 加载第一个、跳过后续并发 warn 日志，避免 LLM 被混淆。

#### Scenario: 同名冲突

- **WHEN** `~/.workhorse-agent/skills/foo/skill.yaml` 与 `~/.workhorse-agent/skills/bar/skill.yaml` 都声明 `name: helper`
- **THEN** 仅前者加载（按目录名字典序），后者跳过；日志 emit `warn { skill: "helper", reason: "duplicate name, skipped" }`

