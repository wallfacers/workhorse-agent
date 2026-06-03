## ADDED Requirements

### Requirement: MCP Adapter 实现 Deferrable

MCP 工具 `Adapter`（`internal/mcp/adapter.go`）SHALL 实现 `Deferrable` 接口。`ShouldDefer()` SHALL 默认返回 true（MCP 工具默认延迟），但当其所属 server 配置了 `always_load: true` 时 SHALL 返回 false。

本要求使 MCP 工具一旦接入 registry 即自动纳入 tool search 延迟，无需改动 tool search 机制本身。本 change 不要求接线 MCP host。

#### Scenario: 默认 MCP 工具可延迟

- **WHEN** 一个 MCP `Adapter` 的所属 server 未设置 `always_load`
- **THEN** 其 `ShouldDefer()` SHALL 返回 true

#### Scenario: always_load 反选延迟

- **WHEN** 一个 MCP `Adapter` 的所属 server 设置了 `always_load: true`
- **THEN** 其 `ShouldDefer()` SHALL 返回 false，该 server 的工具始终全量加载

### Requirement: ServerConfig 支持 always_load

MCP 的 `ServerConfig`（mcp.json 每 server 条目）SHALL 新增可选字段 `always_load`（布尔，默认 false）。该字段为 true 时，该 server 暴露的所有工具不参与 tool search 延迟。

#### Scenario: always_load 默认 false

- **WHEN** mcp.json 的某 server 条目未声明 `always_load`
- **THEN** 该值 SHALL 默认为 false（即其工具默认可延迟）
