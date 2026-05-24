## ADDED Requirements

### Requirement: MCP server 配置与生命周期

服务 SHALL 在启动时读取 `~/.dataagent/mcp.json`，加载并启动启用的 MCP server。

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

### Requirement: Streamable HTTP transport

`transport: "http"` 时 SHALL 按 MCP Streamable HTTP 规范（POST + SSE 配对）与 server 通讯。`auth_header` 字段值附加到 HTTP 请求 Authorization。

#### Scenario: HTTP MCP 工具调用

- **WHEN** LLM 调用一个 HTTP MCP 暴露的工具
- **THEN** 服务向 `<url>/messages` POST `tools/call` JSON-RPC 请求；服务等待对应 SSE 事件返回；翻译为 ToolResult

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
