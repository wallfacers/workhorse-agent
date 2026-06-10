## ADDED Requirements

### Requirement: serve 启动接线 MCP Host 进会话工具面

`workhorse-agent serve` SHALL 在启动时加载 `~/.workhorse-agent/mcp.json` 并启动全局 MCP Host，将每个已连接 server 的工具经 `mcp.NewAdapter` 注册进全局工具 registry，使所有会话的工具面包含 MCP 工具（命名 `<server>__<tool>`），并自动获得既有的权限门（permission Check）、tool search 延迟（Deferrable）与 orchestrator 超时链。

- mcp.json 不存在或 `servers` 为空时 SHALL 静默跳过（无 MCP，不报错）。
- 单个 server 连接/初始化失败 SHALL 仅记录 `WARN` 并跳过该 server，不 SHALL 阻塞 serve 启动。
- 进程收到退出信号时 SHALL 对所有 MCP server 执行 graceful shutdown（与既有"跨 session 生命周期"要求一致）。
- mcp.json 变更仍需重启生效（热加载维持 V2 范围，不在本变更内）。

#### Scenario: HTTP MCP server 工具进入会话

- **WHEN** mcp.json 配置 `{ "name": "dataweave", "enabled": true, "transport": "http", "url": "https://dw.internal/mcp", "auth_header": "Bearer tok" }` 且该 server 暴露工具 `query_tasks`，serve 启动后新建会话
- **THEN** 会话工具面包含 `dataweave__query_tasks`（默认经 ToolSearch 延迟披露），LLM 调用它时请求携带 `Authorization: Bearer tok` 转发到该 server 的 `tools/call`

#### Scenario: 单 server 失败不阻塞启动

- **WHEN** mcp.json 配置两个 server，其中一个连接失败
- **THEN** serve 正常启动并输出 `WARN`；另一个 server 的工具正常注册可用

#### Scenario: 无 mcp.json 静默继续

- **WHEN** `~/.workhorse-agent/mcp.json` 不存在
- **THEN** serve 正常启动，工具面与无 MCP 时完全一致，无错误日志
