# mcp-integration Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: MCP server 配置与生命周期

服务 SHALL 在启动时读取 `~/.workhorse-agent/mcp.json`，加载并启动启用的 MCP server。

配置格式：

```json
{
  "servers": [
    {
      "name": "filesystem",
      "enabled": true,
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "env": { "FOO": "bar" }
    },
    {
      "name": "remote",
      "enabled": true,
      "transport": "http",
      "url": "https://mcp.example.com/v1",
      "auth_header": "Bearer ..."
    }
  ]
}
```

每个 server 的生命周期 SHALL 由 MCP host 管理：启动、初始化握手、`tools/list` 拉取工具、健康监测、graceful shutdown。

#### Scenario: enabled=false 不启动

- **WHEN** mcp.json 中某 server `enabled: false`
- **THEN** 服务启动时不启动该 server，不向 ToolRegistry 注册其工具

### Requirement: stdio transport

`transport: "stdio"` 时 SHALL 启动配置中的 `command + args` 子进程，通过其 stdin/stdout 收发 JSON-RPC 2.0 消息（每条消息一行，UTF-8）。

子进程 stderr SHALL 被服务的结构化日志吸收（`mcp.stderr` 字段）。

#### Scenario: 子进程退出后自动重启

- **WHEN** stdio MCP server 子进程意外退出（exit code ≠ 0）
- **THEN** host 等待 1s 后自动重启该 server，最多重试 3 次；3 次失败后标记为 `unhealthy` 并 emit `error` 事件

### Requirement: Streamable HTTP transport（作为 MCP 客户端）

`transport: "http"` 时 workhorse-agent SHALL 作为 MCP 客户端按 MCP 2025-11-25 Streamable HTTP 规范与 MCP server 通讯：

1. **Initialize**：客户端 POST `<url>` 发 `initialize` JSON-RPC 请求；从 `result.capabilities` 获取 server 能力；从响应 header 读 `Mcp-Session-Id`（如有）；记录 server 声明的 endpoint URI 与 protocol version
2. **POST 调用**：所有后续 `tools/list` / `tools/call` 等都 POST 到同一 endpoint；带 `Mcp-Session-Id` header（若 server 返过）
3. **GET SSE 订阅**：客户端 GET 同 endpoint 开 SSE 流接收 server 主动通知（`notifications/*`）
4. **断线重连**：workhorse-agent 作为客户端 SHALL 在 GET SSE 断开后自动重连，带上 `Last-Event-ID` header；初次重连立即，后续指数退避（1s/3s/10s/30s 封顶）
5. **`auth_header`** 字段值附加到所有 HTTP 请求的 Authorization header

#### Scenario: HTTP MCP 初始化流程

- **WHEN** 启动配置了 HTTP transport 的 MCP server
- **THEN** workhorse-agent 先 POST `initialize`；收到 `result` 后保存 `Mcp-Session-Id`；GET endpoint 开 SSE 流；POST `tools/list`；把每个工具适配并注册

#### Scenario: HTTP MCP 工具调用

- **WHEN** LLM 调用一个 HTTP MCP 暴露的工具
- **THEN** workhorse-agent 向 endpoint POST `tools/call` JSON-RPC 请求，带 `Mcp-Session-Id`；等响应或在 SSE 流上的对应 request_id 结果；翻译为 ToolResult

#### Scenario: MCP client SSE 断线自动重连

- **WHEN** workhorse-agent 与 HTTP MCP server 的 GET SSE 流网络中断
- **THEN** 1 秒后自动重连带 `Last-Event-ID`；连续 3 次失败后退避 3s/10s/30s 重试；期间已发出的 POST 请求按超时配置正常处理

### Requirement: JSON-RPC 2.0 客户端

服务 SHALL 实现 MCP 协议要求的 JSON-RPC 2.0 客户端，至少支持：

- `initialize` / `initialized` 握手
- `tools/list`
- `tools/call`
- `notifications/cancelled`
- 错误响应处理（含 `code`、`message`、`data`）

#### Scenario: tools/call 取消

- **WHEN** 父 ctx 取消时一个 MCP 工具调用正在进行
- **THEN** 客户端发送 `notifications/cancelled` 给 server，并对该 tool_use 返回合成 cancelled tool_result

### Requirement: MCP 工具适配为内部 Tool

每个 MCP server 的工具 SHALL 被适配为内部 `Tool` 接口实例：

- `Name()` = `<server_name>__<tool_name>`（双下划线分隔，避免冲突）
- `Description()` 取自 MCP `tools/list` 返回的 description
- `InputSchema()` 取自 MCP 返回的 JSON Schema
- `IsReadOnly(input)` = `false`（保守默认；除非 MCP server 在 tool metadata 中声明 `readonly: true`）
- `CanRunInParallel(input)` = `false`（保守默认；除非声明）
- `Run` 通过 host 转发到对应 server 的 `tools/call`

#### Scenario: 工具名命名空间

- **WHEN** server `filesystem` 暴露工具 `read_file`
- **THEN** 内部注册名为 `filesystem__read_file`，LLM 看到的 schema 名相同

### Requirement: MCP Host 跨 session 生命周期

MCP server 子进程 SHALL 由全局 MCP Host 管理，与单个 session 生命周期解耦：

- MCP server 在 workhorse-agent 服务启动时按 mcp.json 启动一次；所有 session 共享同一组 server
- MCP server 在 workhorse-agent **进程退出**时统一 graceful shutdown（不在 session DELETE 时关闭）
- MCP server 内部状态（如 filesystem cache）跨 session 共享——这是已知行为，不视为 session 隔离破坏（因为 MCP server 在 process 边界外，本就是共享资源）
- 用户通过编辑 mcp.json 并重启 workhorse-agent 修改 MCP server 集合（MVP 不支持 SIGHUP 热重载，V2 加）

#### Scenario: session DELETE 不关闭 MCP server

- **WHEN** 仅有的 1 个活跃 session 被 DELETE，workhorse-agent 服务继续运行
- **THEN** MCP server 子进程**保持运行**，不被关闭；下一次新建 session 时立即可用

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

