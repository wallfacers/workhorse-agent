## ADDED Requirements

### Requirement: 任务清单内置工具

服务 SHALL 提供一个会话级任务清单内置工具，使 LLM 能够创建、更新和列出当前会话的
任务项。工具 SHALL 经既有 ToolRegistry 注册，并受会话 `AllowedTools` 约束——当某会话
的 `AllowedTools` 未包含该工具时，其 schema SHALL NOT 向 LLM 暴露。

工具的具体形态（单一 `TodoWrite`（整表覆盖）或拆分为 `TaskCreate`/`TaskUpdate`/
`TaskList` 多工具）由 design 定夺，但无论形态如何，最终任务模型 SHALL 满足以下
「任务字段与状态机」「作用域与生命周期」requirement。

#### Scenario: 工具受 AllowedTools 门控

- **WHEN** 会话配置 `AllowedTools` 未包含任务清单工具
- **THEN** Agent 不向 LLM 暴露该工具 schema（经工具注册表的 `AllowedTools` 过滤，
  与 `memory_*` / `session_search` 等内置工具同一门控路径）；`AllowedTools` 为空时
  表示放行全部工具，任务清单工具随之暴露

#### Scenario: 创建任务

- **WHEN** LLM 调用任务清单工具新增一个任务，提供 subject（祈使句标题）
- **THEN** 该任务以 `pending` 状态加入当前会话的清单，并返回包含该任务标识的结果

### Requirement: 任务字段与三态状态机

每个任务 SHALL 至少包含：`subject`（简短祈使句标题）、`status`（枚举
`pending` / `in_progress` / `completed`）；MAY 包含 `description`（待办内容）与
`activeForm`（进行时态描述，用于进度展示）。新建任务的初始 `status` SHALL 为
`pending`。状态流转 SHALL 允许 `pending → in_progress → completed`，工具 SHALL
拒绝未定义的状态值。

#### Scenario: 状态流转

- **WHEN** 一个 `pending` 任务被更新为 `in_progress`，随后更新为 `completed`
- **THEN** 每次更新后列出清单都反映最新状态，且无中间状态丢失

#### Scenario: 非法状态被拒绝

- **WHEN** 调用工具将某任务置为枚举外的状态值
- **THEN** 返回 `is_error: true` 的结果，且该任务状态保持不变

### Requirement: 任务清单作用域与生命周期

任务清单 SHALL 为**会话级**：每个会话拥有独立清单，互不可见；子 Agent（Dispatch
派生的会话）拥有自身独立清单，不与父会话共享。清单的持久化策略（纯内存 vs 落
events 表）由 design 定夺；若持久化，SHALL 遵循 events 表 append-only 与 ULID 约定。

#### Scenario: 会话隔离

- **WHEN** 会话 A 与会话 B 各自创建任务
- **THEN** 列出 A 的清单只含 A 的任务，列出 B 的清单只含 B 的任务

### Requirement: 任务变更对用户可见

任务清单的创建与状态变更 SHALL 通过会话的 SSE 通道向客户端广播，使用户可见整体进度。
广播事件的具体名称与负载结构由 design 与 `api-protocol` 对齐。

#### Scenario: 状态变更广播

- **WHEN** 一个任务从 `pending` 变为 `in_progress`
- **THEN** 会话 SSE 流中出现一条携带该任务最新状态的事件

### Requirement: 编排者提示词引导使用任务清单

编排者默认提示词（`DefaultBasePrompt`）SHALL 包含使用任务清单工具的引导：对需要
3 步及以上的复杂任务先建任务清单；开始某步前将其置 `in_progress`；完成后及时置
`completed`，不要批量补记；单步、琐碎任务不必使用。

#### Scenario: 默认提示词含引导

- **WHEN** 检视渲染后的 `DefaultBasePrompt`
- **THEN** 其中包含关于何时及如何使用任务清单工具的文字引导
