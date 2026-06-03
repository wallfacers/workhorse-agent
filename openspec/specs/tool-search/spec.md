# tool-search Specification

## Purpose
TBD - created by archiving change mcp-tool-search. Update Purpose after archive.
## Requirements
### Requirement: 可延迟工具与 Deferrable 接口

系统 SHALL 提供一个可选的 `Deferrable` 接口，工具实现 `ShouldDefer() bool` 即可声明自身为"可延迟"。一个工具被视为可延迟，当且仅当它实现 `Deferrable` 且 `ShouldDefer()` 返回 true，且其名字不是 `ToolSearch`。

未实现 `Deferrable` 的工具 SHALL NOT 被延迟——其完整 schema 始终出现在发给模型的初始 tool 列表中。

`ToolSearch` 工具自身 SHALL NOT 被延迟（否则模型无法 bootstrap 发现其他工具）。

#### Scenario: 实现 Deferrable 的工具被识别为可延迟

- **WHEN** 一个工具实现 `Deferrable` 且 `ShouldDefer()` 返回 true
- **THEN** 在延迟生效时该工具的完整 schema SHALL 被排除出初始 tool 列表，仅保留其名字

#### Scenario: 未实现 Deferrable 的工具始终全量加载

- **WHEN** 一个工具（如 `Read`、`Bash`、`Dispatch`）未实现 `Deferrable`
- **THEN** 无论 tool search 模式如何，其完整 schema SHALL 始终出现在初始 tool 列表中

### Requirement: ToolSearch 工具按需发现

系统 SHALL 提供内建工具 `ToolSearch`，输入为 `{"query": string, "max_results": int}`（`max_results` 默认 5），用于按需取回可延迟工具的完整 schema。其 `Description()` SHALL 为英文。

`ToolSearch` SHALL 支持三种 query 形式：

- `select:A,B,C`：按名字精确多选（逗号分隔）。
- `+term`：以 `+` 前缀的词为必选项，候选工具的名字或描述 SHALL 全部包含所有必选项后再按其余词打分。
- 纯关键词：对候选工具按名字分段命中与描述词边界命中加权打分，返回至多 `max_results` 个最高分工具。

工具名 SHALL 按 `__` 与 `_` 拆分为可搜索分段（如 `slack__send` → `slack`、`send`），不依赖 `mcp__` 前缀。

`ToolSearch` 的搜索范围 SHALL 限定为当前会话的**可延迟工具集**（经会话 AllowedTools 过滤后仍可延迟者）。

#### Scenario: select 精确多选

- **WHEN** 模型调用 `ToolSearch` 且 `query` 为 `select:slack__send,github__pr`
- **THEN** 结果 SHALL 包含这两个工具的完整 schema（两者均在可延迟集内时）

#### Scenario: 关键词搜索按相关性排序

- **WHEN** 模型调用 `ToolSearch` 且 `query` 为 `send message slack`
- **THEN** 名字或描述与 `slack`/`send`/`message` 命中更强的工具 SHALL 排在更前，结果数不超过 `max_results`

#### Scenario: 必选项过滤

- **WHEN** `query` 为 `+slack send`
- **THEN** 仅名字或描述包含 `slack` 的工具进入候选，再按 `send` 打分排序

#### Scenario: 无命中

- **WHEN** `query` 无任何可延迟工具命中
- **THEN** `ToolSearch` SHALL 返回一条说明"未找到匹配的延迟工具"的结果，且不标记任何工具为已发现

### Requirement: ToolSearch 结果与发现标记

`ToolSearch` 命中后 SHALL 返回一个 `<functions>` 文本块，其中每个命中工具渲染为一行 `{"description": ..., "name": ..., "parameters": <inputSchema>}`，编码与初始 tool 列表一致，使模型据此构造调用。

同时，`ToolSearch` 的结果 SHALL 通过 `Result.Modifier` 将所有命中工具名标记为当前会话"已发现"。结果 SHALL 额外携带机器可读的命中名字段（如 `matches` 数组），以支持会话恢复时的重建。

#### Scenario: 命中即返回 schema 并标记发现

- **WHEN** `ToolSearch` 命中工具 `slack__send`
- **THEN** 结果文本 SHALL 含 `slack__send` 的完整 `parameters` schema，且会话的已发现集合在本批次结算后 SHALL 包含 `slack__send`

### Requirement: 已发现工具集合的生命周期

会话 SHALL 持有一个"已发现工具"集合。一旦某可延迟工具被 `ToolSearch` 命中并标记，其完整 schema SHALL 在后续每一轮出现在发给模型的 tool 列表中（直至会话结束）。

该集合 SHALL 跨 compaction 存活（不随历史裁剪丢失）。

会话从持久化恢复（rehydration）时，系统 SHALL 通过扫描历史中已持久化的 `ToolSearch` 结果重建该集合，以保证恢复后模型对已发现工具的调用不落空。

#### Scenario: 发现后持续可调用

- **WHEN** 模型在第 N 轮通过 `ToolSearch` 发现 `slack__send`，并在第 N+2 轮调用它
- **THEN** 第 N+1、N+2 轮的 tool 列表 SHALL 持续包含 `slack__send` 的完整 schema

#### Scenario: compaction 后仍可调用

- **WHEN** 已发现 `slack__send` 后发生历史 compaction
- **THEN** compaction 之后的 tool 列表 SHALL 仍包含 `slack__send`

#### Scenario: 会话恢复后重建发现集

- **WHEN** 某会话曾发现 `slack__send`，进程重启后该会话被 rehydrate
- **THEN** 系统 SHALL 从历史中的 `ToolSearch` 结果重建已发现集，使 `slack__send` 继续出现在 tool 列表中

### Requirement: 延迟工具公告

当延迟生效时，系统 SHALL 在每轮请求中向模型告知"可延迟但尚未发现"的工具名清单，形如 `<available-deferred-tools>` 块。该公告 SHALL 注入请求消息尾部，SHALL NOT 并入用于 prompt cache 的稳定系统提示前缀。

已被发现的工具 SHALL NOT 出现在该公告中（它们已以完整 schema 出现在 tool 列表）。

#### Scenario: 公告列出未发现的延迟工具

- **WHEN** 存在 5 个可延迟工具，其中 2 个已发现
- **THEN** 公告 SHALL 只列出其余 3 个未发现工具的名字

### Requirement: Tool search 三档模式与阈值

系统 SHALL 由配置 `tools.tool_search` 决定延迟行为，语义对齐 Claude Code：

- `tst`（或空值，**默认**）：永远延迟所有可延迟工具。
- `auto`：仅当可延迟工具的体量 ≥ 上下文规模的 10% 时才延迟。
- `auto:N`：同 `auto`，阈值百分比改为 N（钳制到 0-100）；`auto:0` 等价 `tst`，`auto:100` 等价 `standard`。
- `standard`：从不延迟，行为与未引入本特性时一致。

`auto` 模式的体量估算 SHALL 采用 chars/4 量级的启发式，对每个可延迟工具的 `name + description + inputSchema` 求和；阈值基准 SHALL 取 `agent.max_history_tokens` 的对应百分比。

#### Scenario: 默认 tst 永远延迟

- **WHEN** `tools.tool_search` 未配置且存在可延迟工具
- **THEN** 所有可延迟工具默认被延迟

#### Scenario: standard 模式零回归

- **WHEN** `tools.tool_search` 为 `standard`
- **THEN** 所有工具（含可延迟工具）的完整 schema SHALL 全量出现在初始 tool 列表，且不注入公告、不要求 `ToolSearch`

#### Scenario: auto 低于阈值不延迟

- **WHEN** `tools.tool_search` 为 `auto` 且可延迟工具体量低于上下文 10%
- **THEN** 这些工具 SHALL 全量加载，不被延迟

