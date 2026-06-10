# Proposal: support-dataweave-headless-integration

## Why

workhorse-agent 将作为 DataWeave 数据中台的中央 Agent 大脑以 headless server 方式接入：dataweave-api 暴露 MCP Streamable HTTP server 提供平台工具（大量高频只读工具 + 可逆写 + 不可逆危险操作），并桥接 AG-UI 前端与 workhorse 会话 SSE 流。对照集成需求 R1–R8 的差距分析显示，权限审批通道（R2）、审计导出（R5）、长会话稳定性（R7）、多会话隔离（R8）已基本满足，但存在五个阻断性/体验性缺口：

1. **MCP host 未接线**（最大缺口）：`internal/mcp` 的 `Host`/`Adapter`/HTTP transport 全部就绪，但 `NewHost`/`LoadAndStart` 在 `internal/mcp` 之外零调用——MCP 工具今天根本进不了任何会话的 registry，R1/R6 整体被阻断。
2. **权限规则 tool 字段仅精确匹配**（`internal/permission/manager.go:297-307`）：无法表达 `dataweave__query_*` 这类按名免打扰规则；只读工具数量多且会演进，逐个写精确规则不可维护。
3. **permission_request 事件语义混乱**：agent loop 对**每个**工具调用都发 `permission_request`（`internal/agent/loop.go:755`，即使规则已自动放行），prompt 回调又用不同的 request_id 再发一次（`cmd/workhorse-agent/cmd_serve.go:472`）；事件缺 `expires_at`；决议结果（谁批的/规则放行/超时拒绝）没有任何事件——外部审批 UI 无法可靠区分"需要人工决策"与"已自动放行"，审计链路缺最后一环。
4. **会话无法注入 instructions / metadata**：`createSessionRequest`（`internal/api/sessions.go:18-27`）不接受自定义指令与元数据，DataWeave 无法传入页面上下文（taskId/instanceId）和 conversationId 关联。
5. **`tools.default_allowed_tools` 配置字段已定义但未实施**（`internal/config/config.go:94`，全仓库无引用）：headless 部署无法在服务器级声明默认工具画像（关掉内置 Bash/Write/Edit），只能依赖每次建会话时显式传 `allowed_tools`。

## What Changes

- **MCP host 接线**：`serve` 启动时加载 `~/.workhorse-agent/mcp.json` 并启动全局 MCP Host；会话级 registry 构建时把 `host.AllTools()` 经 `mcp.NewAdapter` 注册进去（命名 `<server>__<tool>`，自动获得 tool search 延迟、权限门、orchestrator 超时链）。进程退出时 graceful shutdown。
- **权限规则 tool 字段 glob 匹配**：`matchToolResource` 的 tool 比较从精确匹配改为复用 `MatchGlob`（无 glob 元字符的既有规则语义不变，向后兼容）；preset_rules、`POST /v1/permissions`、`PUT /v1/permission-config` 的 tool 字段同步获得 glob 能力。deny 优先级语义保持不变（`DenyPermanent` 先于任何 allow）。
- **权限审批事件生命周期**：
  - `permission_request` 仅在**真正需要外部决策**（无规则命中、进入 prompt）时发射一次，request_id 统一为 tool_use id，payload 增加 `expires_at`；移除 loop 中无条件的预发射。
  - 新增 SSE 事件 `permission_resolved`：每次权限检查结束后发射，含 `request_id`（=tool_use id）、`tool`、`decision`、`source`（`rule` | `prompt` | `timeout` | `default`）。超时拒绝因此在流中可观察（R2），审批审计链路闭环（R5）。
- **会话定制**：`POST /v1/sessions` 新增 `instructions`（string，注入 system prompt 的 Instructions 段，不污染缓存前缀）与 `metadata`（string→string map，持久化并在 `GET /v1/sessions/{id}` 返回）。
- **`tools.default_allowed_tools` 生效**：建会话未显式传 `allowed_tools` 时回退到该配置值，实现服务器级默认工具画像。
- **工具白名单条目支持 glob**：`allowed_tools` / `default_allowed_tools` 条目含 glob 元字符时按 glob 匹配工具名（`dataweave__*` 一行放行整个 MCP server，server 新增工具无需改配置）；无元字符条目保持精确匹配，零行为变化。被白名单过滤掉的已注册工具在会话创建日志中可观察，避免静默消失。

**BREAKING**（事件语义）：`permission_request` 不再对每个工具调用无条件发射，且 request_id 从"loop 用 tool_use id / 回调用 ULID 各发一次"统一为单次发射、tool_use id。已知唯一消费者为本仓库测试与参考 UI，一并更新。

## Capabilities

### New Capabilities

（无 —— 全部缺口落在既有 capability 的需求修订上）

### Modified Capabilities

- `mcp-integration`：新增"serve 启动接线 MCP Host 并注册工具进会话 registry"要求（原 spec 显式标注接线不在范围内，本变更补上）；mcp.json 热加载维持 V2 不变。
- `permission-control`：权限规则匹配语义中 tool 字段从精确匹配改为 glob 匹配；新增权限决议可观察性要求（`permission_resolved`）。
- `permission-preset`：预设规则格式说明中 tool 字段允许 glob 模式。
- `api-protocol`：Server → Client 事件类型变更——`permission_request` 触发条件收窄 + payload 增加 `expires_at`；新增 `permission_resolved` 事件；`POST /v1/sessions` 请求体新增 `instructions`/`metadata` 字段，`SessionMeta` 返回 `metadata`。
- `session-management`：会话创建接受并持久化 instructions 与 metadata；instructions 进入 system prompt 装配的动态段。
- `configuration`：`tools.default_allowed_tools` 从"已定义未实施"变为强制生效的服务器级默认工具白名单，条目支持 glob。
- `tool-system`：会话级 `AllowedTools` 条目支持 glob 匹配工具名；白名单过滤结果在日志可观察。

## Impact

- `cmd/workhorse-agent/cmd_serve.go` — MCP host 启动/关闭接线；会话 registry 注册 MCP adapter；prompt 回调改用传入的 request_id 并停止重复发射 `permission_request`
- `internal/permission/manager.go` + `glob.go` — `matchToolResource` tool 字段 glob；`Check` 路径向调用方暴露决议来源（source）
- `internal/agent/loop.go` — `checkPermissions` 移除无条件 `permission_request` 发射，新增 `permission_resolved` 发射；将 tool_use id 传入权限检查
- `internal/api/protocol/protocol.go` — 新事件常量 `permission_resolved`
- `internal/api/sessions.go`、`internal/session/`、`internal/store/`（types + sqlite 迁移）— `instructions`/`metadata` 字段链路；`default_allowed_tools` 回退
- `internal/prompt/` — instructions 段拼接（复用既有 `Instructions` 输入位）
- 既有测试更新：依赖旧 `permission_request` 预发射语义的 loop/api 测试
- DataWeave 桥接层（外部）：按 design 中的 JSON 示例对接 `permission_request`/`permission_resolved`/会话创建字段
- 非目标：mcp.json 热加载与运行期 MCP 注册 API（spec 已标 V2；DataWeave 端点固定，重启可接受）；DangerousCommandGuard 扩展到 MCP 工具（已验证对 MCP 不误伤；`node_exec` 强审批通过"不配 allow 规则 → 必然走 prompt"即可达成）
