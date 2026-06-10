## 1. 权限规则 tool 字段 glob 匹配

- [x] 1.1 `internal/permission/manager.go` `matchToolResource`：tool 比较改为 `r.tool != "" && !MatchGlob(r.tool, tool)`；同步更新函数注释
- [x] 1.2 `internal/permission/manager_test.go`：新增用例——`dataweave__query_*` 命中 `dataweave__query_tasks`；`dataweave__*` allow + `dataweave__node_exec` deny_permanent 并存时 deny 胜出；无元字符规则行为不变；`*` 匹配任意工具
- [x] 1.3 检查 `internal/api/permissions.go` 与 `permission_config.go` 对 tool 字段无额外精确校验阻碍 glob 值（如有则放开）

## 2. 权限事件生命周期（permission_request 收窄 + permission_resolved）

- [x] 2.1 `internal/permission/manager.go`：`Request` 增加 `RequestID string`；`Check` 签名改为接收 `RequestID` 并返回 `(Decision, Source, error)`，`Source` ∈ `rule|default|prompt|timeout|none`（dangerous 强制询问归 `prompt`/`timeout`）
- [x] 2.2 `internal/agent/loop.go` `checkPermissions`：删除无条件 `permission_request` 发射；将 `c.ID` 作为 RequestID 传入 Check；Check 返回后发射 `permission_resolved { request_id, tool, decision, source }`
- [x] 2.3 `cmd/workhorse-agent/cmd_serve.go` `permissionPromptUsingSessions`：改用 `req.RequestID`（删除 ULID 生成）；事件 payload 增加 `expires_at`（从 `ctx.Deadline()` 读取，RFC3339）
- [x] 2.4 `internal/api/protocol/protocol.go`：新增 `EventPermissionResolved ServerEventType = "permission_resolved"`
- [x] 2.5 更新依赖旧预发射语义的测试（`internal/agent/loop_test.go`、`internal/api` 流测试）；新增超时场景断言 `permission_resolved { source: "timeout" }`
- [x] 2.6 `internal/version`（或 protocol_version 所在处）：提升 protocol_version；参考 Web UI（如消费 permission_request）同步适配

## 3. MCP host 接线

- [x] 3.1 `cmd/workhorse-agent/cmd_serve.go`：启动时 `mcp.NewHost` + `LoadAndStart(configDir/mcp.json)`（文件缺失静默跳过；单 server 失败 WARN 跳过），退出路径挂 graceful shutdown
- [x] 3.2 `cmd/workhorse-agent/cmd_serve.go`：对 `host.AllTools()` 逐个 `registry.Register(mcp.NewAdapter(st))`，置于内置工具注册之后；确认与 tool search 延迟、`buildProviderToolSchemas` 协同
- [x] 3.3 集成测试：内存/httptest MCP server → serve 启动 → 新建会话工具面含 `<server>__<tool>`；HTTP transport 携带 `auth_header`；单 server 失败不阻塞启动
- [x] 3.4 端到端验证权限链：对 MCP 工具名配 preset glob 规则后调用免询问（对应 permission-control 新场景）

## 4. 会话 instructions / metadata

- [x] 4.1 `internal/store/sqlite/migrations.go`：v6 迁移——sessions 表加 `instructions TEXT NOT NULL DEFAULT ''`、`metadata_json TEXT NOT NULL DEFAULT ''`
- [x] 4.2 `internal/store/types.go` + `internal/store/sqlite/crud.go`：`Session` 增加 `Instructions`、`MetadataJSON`，CRUD 读写新列
- [x] 4.3 `internal/session/session.go` + `manager.go`：`Options` 增加 `Instructions`、`Metadata map[string]string`；持久化与水合路径带上两字段
- [x] 4.4 `internal/api/sessions.go`：`createSessionRequest` 增加 `instructions`/`metadata` + 限额校验（16 KiB / 32 键、键值 1 KiB，超限 400）；`sessionMeta` 增加 `metadata`（camelCase，空时省略）
- [x] 4.5 instructions 注入：在 loop 装配处（`instructions.Block` 结果之后）拼接会话级 instructions——改 `internal/agent/loop.go:445-450` 装配输入或 `instructions.Block` 调用方，确保不进缓存前缀
- [x] 4.6 测试：创建带两字段的会话 → GET 返回 metadata；水合后 instructions 仍生效（fake provider 断言 system prompt 含文本且 base 前缀不变）；超限 400

## 5. 工具白名单：default_allowed_tools 生效 + 条目 glob

- [x] 5.1 `internal/api/sessions.go` `handleCreateSession`：请求 `allowed_tools` 为空时回退 `cfg.Tools.DefaultAllowedTools`（API server 需可访问该配置值——经构造参数传入）
- [x] 5.2 `internal/tools/registry.go` `Filtered`：条目含 glob 元字符（`*?[`）时用 `path.Match` 匹配工具名，否则精确匹配；nil/空列表语义不变
- [x] 5.3 会话创建路径（`cmd_serve.go` 或 `sessions.go`）：记录"已注册但被白名单过滤"的工具名日志（DEBUG 级以上），消除静默消失
- [x] 5.4 测试：配置默认白名单后不带 `allowed_tools` 建会话 → 工具 schema 不含 Bash/Write/Edit；`dataweave__*` 条目匹配该前缀全部工具且新注册工具自动在列；显式传非空列表覆盖默认；无元字符条目行为与现状逐字节一致；过滤日志存在

## 6. 收尾

- [x] 6.1 `golangci-lint run` + `go test ./...` 全绿（lint 在本变更触及文件上零新增发现；test/e2e、sessionsearch、real_e2e 存量发现与本变更无关）
- [x] 6.2 更新 CLAUDE.md：MCP host 已接线（删除"wiring is out of scope"表述）、权限事件新语义、会话新字段、default_allowed_tools 生效
- [x] 6.3 真实链路验证（自动化线缆级 e2e，等价 curl 流程，`test/e2e/permission_approval_test.go`）：preset glob 免打扰（规则放行 0 个 `permission_request`，`permission_resolved{source:rule}`）；危险 MCP 工具 → SSE `permission_request`（含 expires_at，request_id=tool_use id）→ 纯 HTTP POST 决策 → `permission_resolved{source:prompt}` → `tool_call_start`；超时 → `permission_resolved{source:"timeout"}` 且工具不执行
