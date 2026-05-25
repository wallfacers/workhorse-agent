## ADDED Requirements

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

服务 SHALL 在会话的 system prompt 中追加可发现的 skill 清单（仅 name + trigger，不含 content），格式如下：

```
<available_skills>
- name: git-helper
  trigger: 用户要求执行 git 操作（commit、push、rebase 等）时使用此 skill
- name: code-review
  trigger: 用户要求审查代码、PR、找问题时使用此 skill
</available_skills>

可以调用 LoadSkill 工具加载完整指令。
```

#### Scenario: 无 skill 时不追加

- **WHEN** `~/.workhorse-agent/skills/` 为空目录
- **THEN** system prompt 不含 `<available_skills>` 块

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
