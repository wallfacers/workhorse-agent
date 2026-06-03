## ADDED Requirements

### Requirement: 延迟生效时 tool 列表仅暴露可延迟工具的名字

每轮组装发给模型的 tool 列表时，若 tool search 延迟生效（见 `tool-search` 能力的模式判定），系统 SHALL 对"可延迟、且当前会话尚未发现、且非 `ToolSearch`"的工具仅暴露其名字（经公告通道），SHALL NOT 在 tool 列表中包含其完整 schema。其余工具——非可延迟工具、已发现的可延迟工具、`ToolSearch` 本身——SHALL 照常以完整 schema 出现。

延迟过滤 SHALL 发生在现有的"按会话 AllowedTools 过滤"之后，即被会话权限排除的工具不会因延迟逻辑重新出现。

#### Scenario: 延迟生效时混合列表

- **WHEN** 会话工具集含 `Read`（非可延迟）、`slack__send`（可延迟未发现）、`github__pr`（可延迟已发现），且延迟生效
- **THEN** tool 列表 SHALL 含 `Read`、`github__pr`、`ToolSearch` 的完整 schema，且 `slack__send` 仅以名字出现在公告中

#### Scenario: AllowedTools 优先于延迟逻辑

- **WHEN** 某可延迟工具已被会话 AllowedTools 排除
- **THEN** 它既不出现在 tool 列表，也不出现在延迟公告中

### Requirement: 工具目录暴露给 ToolSearch

系统 SHALL 通过工具执行环境（`Env`）向 `ToolSearch` 暴露当前会话可延迟工具的 `name`、`description` 与 `inputSchema`，使其打分与 schema 渲染所见集合与 tool 列表组装侧一致。该暴露 SHALL 采用本仓库既有的"`any` 字段 + 类型断言"惯例以避免 import cycle。

#### Scenario: ToolSearch 看到的集合与组装侧一致

- **WHEN** 某轮可延迟工具集为 {A, B, C}
- **THEN** 同轮 `ToolSearch` 经 `Env` 读取的可延迟工具目录 SHALL 恰为 {A, B, C} 的 name/description/inputSchema

### Requirement: 本地工具描述必须为英文

所有本地（非 MCP 来源、描述为我方静态字符串）工具的 `Description()` 返回值 SHALL 为英文：仅含拉丁字母。排版标点（em dash、弯引号等非字母符号）允许；非拉丁字母（CJK、西里尔等）SHALL NOT 出现。该约束 SHALL 由自动化测试守门。MCP 来源工具与客户端提供的 frontend 工具描述不受此约束。

#### Scenario: 含非拉丁字母的本地描述被测试拒绝

- **WHEN** 某本地工具的 `Description()` 含非拉丁字母（如中文）
- **THEN** 描述约束测试 SHALL 失败
