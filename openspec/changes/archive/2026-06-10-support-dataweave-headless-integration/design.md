# Design: support-dataweave-headless-integration

## Context

DataWeave 集成的差距分析（详见 proposal）确认五个缺口。本设计给出每个缺口的最小实现方案，并为 DataWeave 桥接层提供所有新增/变更 API 与事件的 JSON 契约。

现状关键事实（已验证）：

- MCP 侧 `Host.LoadAndStart`（`internal/mcp/host.go:65`）、`NewAdapter`（`adapter.go:27`，命名 `<server>__<tool>`）、HTTP transport（`transport_http.go`，`AuthHeader` 应用于 POST 与 SSE GET）全部就绪，仅缺 `cmd_serve.go` 的接线。
- 权限匹配 `matchToolResource`（`internal/permission/manager.go:297-307`）：tool 精确匹配 + pattern glob（`MatchGlob`，`glob.go`）。`matchPermanentRule` 已实现 deny 优先于 allow（`manager.go:196-205`）。
- `Manager.Check` 决策次序（`manager.go:127-158`）：dangerous → session 规则 → permanent 规则 → `default_permission` → prompt（超时 `ErrTimeout` → loop 按 deny 合成 tool_result）。
- `loop.checkPermissions`（`loop.go:729-776`）对每个非 InternalGated 工具调用**无条件**发 `permission_request`（request_id = tool_use id）；serve 的 prompt 回调（`cmd_serve.go:461-498`）在真正 prompt 时**再次**发射（request_id = 新 ULID）。决策应答经 `POST /v1/sessions/{id}/stream` 的 `permission_decision` 消息（`stream_post.go:114-140`）。
- 所有 SSE 事件持久化（events 表，`Last-Event-ID` 续传，`stream_get.go:74-96`）；`tool_call_start` 含完整 input，`tool_call_done` 含截断标记 `[truncated: kept N bytes of M]` 的 output；`GET /v1/sessions/{id}/history` 提供全量回放。
- `createSessionRequest`（`internal/api/sessions.go:18-27`）无 instructions/metadata；`tools.default_allowed_tools`（`config.go:94`）无引用。
- sqlite 迁移当前至 v5（`migrations.go:158-163`）。

## Goals / Non-Goals

**Goals:**

1. MCP 工具进入会话工具面（接线 host → registry），DataWeave 平台工具可被 LLM 调用并自动获得权限门、tool search 延迟、orchestrator 超时链。
2. 一条 preset 规则即可对 `dataweave__query_*` 全量免打扰；连续 20 个只读 MCP 调用 0 次权限询问、0 个 `permission_request` 事件。
3. 纯 HTTP/SSE 走通外部审批闭环，且审批生命周期（请求 → 决议/超时）在事件流中完整可审计。
4. DataWeave 能按会话注入页面上下文 instructions 与 conversationId 等 metadata。
5. headless 部署可在 config 一处声明默认工具画像。

**Non-Goals:**

- mcp.json 热加载 / 运行期 MCP 注册 API（mcp-integration spec 已标 V2；DataWeave MCP 端点固定）。
- DangerousCommandGuard 扩展到 MCP 工具（对 MCP 不误伤已验证）。DataWeave 平台工具的**细粒度危险判定与审批不在 workhorse 侧**：命令串解析、资源归属（dev/prod、是否本平台提交的 app）只有平台查得到，由 DataWeave MCP server 内部的 PolicyEngine 承担（判 L2/L3 → 返回 `PENDING_APPROVAL` + 单号 → agent 透传审批卡片 → 人批后调 `approve_and_execute`）。workhorse 的 permission prompt 通道保留，用于内置工具兜底与未来其他集成方。
- 审批人身份（approver identity）落 workhorse 侧——审批人由 DataWeave 在自己侧关联 request_id 记录，workhorse 只记录决策与来源。
- 权限规则 resource/pattern 语义变更（保持现状）。
- 逐消息页面上下文（用户当前所在 taskId/instanceId 随导航变化）：属于桥接层职责——dataweave-api 把上下文拼进**每条用户消息文本**（如 `[当前上下文: /ops, instance #100]\n<用户消息>`）。会话级 `instructions` 只承载会话生命周期内稳定的指令；workhorse 对逐消息上下文零改动。

## Decisions

### D1 · MCP host 接线位置与生命周期

`serve` 启动时（provider registry 之后、`RunnerFactory` 构造之前）：

```go
mcpHost := mcp.NewHost(logger)
mcpHost.LoadAndStart(ctx, filepath.Join(configDir, "mcp.json"))   // 文件缺失/空 = 无 MCP，静默继续
defer mcpHost.Shutdown()
```

工具注册走**全局 registry**（`cmd_serve.go:139` 构建处）：对 `mcpHost.AllTools()` 逐个 `registry.Register(mcp.NewAdapter(st))`。理由：

- mcp-integration spec 已规定 host 跨 session、进程级共享；全局 registry 与之一致。
- Adapter 已实现 `Deferrable`，注册即自动纳入 tool search 延迟，大工具面不膨胀上下文。
- 备选方案（每会话 registry overlay，如 adapter-generator 的做法）被否：MCP 工具集对所有会话相同，无会话差异，overlay 徒增复杂度。

启动失败语义：单个 server 连接失败 → `WARN` 日志 + 跳过该 server（不阻塞 serve 启动）；与 smoke-test 失败排除 external agent 的既有姿态一致。

### D2 · tool 字段 glob：复用 `MatchGlob`，不引入新语法

`matchToolResource` 中 `r.tool != tool` 改为：

```go
if r.tool != "" && !MatchGlob(r.tool, tool) {
    return false
}
```

- 向后兼容：不含 `*?[` 的既有规则在 `MatchGlob` 下与字面比较等价（`MatchGlob` 的 `*` 不跨 `/`，工具名不含 `/`，无歧义）。
- `dataweave__query_*`、`dataweave__*` 均可表达；`deny_permanent` 优先级既有逻辑（`manager.go:196-205`）保证 `allow dataweave__*` + `deny_permanent dataweave__node_exec` 并存时 deny 胜出。
- 备选（仅支持后缀 `*`）被否：`MatchGlob` 已存在、已测试，引入子集语法反而多一套规则。
- 同步点：preset_rules / `POST /v1/permissions` / `PUT /v1/permission-config` 共用同一 store 行与匹配函数，无需各自改动；仅 spec 文案与校验放开。

### D3 · 权限事件生命周期：单一 request 事件 + 决议事件

**request_id 统一为 tool_use id**，贯穿 `tool_call_start` / `permission_request` / `permission_resolved` / DataWeave 审计表，免去外部关联表。

实现：

1. `permission.Request` 增加 `RequestID string` 字段；`Manager.Check` 签名改为 `Check(ctx, CheckInput{SessionID, RequestID, Tool, Resource})`，返回 `(Decision, Source, error)`。`Source ∈ {rule, default, prompt, timeout, none}`（`none` = 无 prompt 回调时的兜底 deny）。内部 API，调用方仅 loop 与测试。
2. `loop.checkPermissions` **删除**无条件 `permission_request` 发射（`loop.go:755-759`）；`StateAwaitPerm` 转换保留但延后到 Manager 真正进入 prompt 之后——通过 prompt 回调发射事件这一事实本身界定（state 转换移入回调前的现有位置即可，行为面不变：只在真的等待审批时才处于 `await_perm`）。
3. serve 的 prompt 回调（`cmd_serve.go:461-498`）改用 `req.RequestID`（不再生成 ULID），并从 `ctx.Deadline()` 读出 `expires_at` 放入事件——Manager 的 `WithTimeout` 已把 deadline 挂在传给回调的 ctx 上，无需新传参。
4. `Check` 返回后由 loop 发射 `permission_resolved`（无论来源是规则、默认值、prompt 还是超时），保证每个经过权限门的调用都有决议记录。

**事件 JSON 契约**（DataWeave 桥接层按此实现）：

`permission_request` —— 仅当需要外部决策时发射一次：

```json
{
  "type": "permission_request",
  "payload": {
    "request_id": "toolu_01XYZ...",
    "tool": "dataweave__publish_workflow",
    "resource": "",
    "dangerous": false,
    "reason": "",
    "expires_at": "2026-06-10T12:05:00.000Z"
  }
}
```

决策应答（既有通道，不变）—— `POST /v1/sessions/{id}/stream`。注意客户端消息是**扁平**结构（`type` 与字段同级，无嵌套 `payload` 包裹；服务端 `DecodeClientMessage` 把整个 body 作为 payload 解码）：

```json
{
  "type": "permission_decision",
  "request_id": "toolu_01XYZ...",
  "decision": "allow_once"
}
```

成功返回 `202`（无 body）；会话不在 `await_perm` 状态时返回 `409`。

`decision` 取值：`allow_once | allow_session | allow_permanent | deny | deny_permanent`。

`permission_resolved` —— 每次权限检查结束发射：

```json
{
  "type": "permission_resolved",
  "payload": {
    "request_id": "toolu_01XYZ...",
    "tool": "dataweave__publish_workflow",
    "decision": "allow_once",
    "source": "prompt"
  }
}
```

超时示例：`{"decision": "deny", "source": "timeout"}`。规则免打扰放行：`{"decision": "allow_permanent", "source": "rule"}`（此时**没有**对应的 `permission_request` 事件）。

- 备选（保留 loop 预发射、新增 `pending` 字段区分）被否：两个同名事件、两套 request_id 是当前混乱的根源，收窄触发条件才能让外部 UI 用"收到 `permission_request` = 必须弹审批"这一简单契约工作。
- BREAKING 面：本仓库 loop/api 测试与参考 UI 同步更新；事件协议版本由 `/health` 的 `protocol_version` 表达。

### D4 · 会话 instructions / metadata

`POST /v1/sessions` 新增字段：

```json
{
  "workdir": "/srv/dataweave/agent-workdir",
  "provider": "anthropic",
  "model": "claude-sonnet-4-6",
  "allowed_tools": ["Read", "Grep", "ToolSearch", "memory_read", "session_search"],
  "instructions": "当前页面上下文：taskId=T-1024, instanceId=I-88。回答中引用平台对象时使用其 ID。",
  "metadata": {
    "dataweave_conversation_id": "conv-7f3a",
    "dataweave_user": "u-1001",
    "page": "task-detail"
  }
}
```

- `instructions`（≤16 KiB，超限 400）注入 `prompt.BuildSystemPrompt` 的 `Instructions` 段尾部（`loop.go:445-450` 装配点，与 AGENTS.md 内容拼接）——位于动态段，不污染 Anthropic 缓存前缀（base-first 顺序不变）。
- `metadata`（string→string，≤32 键、键值各 ≤1 KiB）持久化 sessions 表新列 `metadata_json`（v6 迁移），`GET /v1/sessions/{id}` 的 `SessionMeta` 增加 `metadata` 字段原样返回。workhorse 不解释其内容。
- instructions 同样持久化（v6 列 `instructions`），会话水合后继续生效（满足"持久化会话的按需水合"既有要求）。
- 备选（metadata 复用 env）被否：env 会进入 Bash 子进程环境与 EnvSnapshot，语义错误且有泄露面。

`SessionMeta` 返回示例（新增字段加粗部分）：

```json
{
  "id": "01JX...",
  "status": "idle",
  "workdir": "/srv/dataweave/agent-workdir",
  "provider": "anthropic",
  "model": "claude-sonnet-4-6",
  "metadata": { "dataweave_conversation_id": "conv-7f3a" },
  "createdAt": "2026-06-10T12:00:00.000Z"
}
```

### D5 · `tools.default_allowed_tools` 生效点

`handleCreateSession`（`internal/api/sessions.go`）中：请求 `allowed_tools` 为空（nil 或零长）→ 回退 `cfg.Tools.DefaultAllowedTools`。API 层回退而非 registry 层过滤，理由：会话仍可显式传更宽/更窄的列表覆盖（Dispatch 子代理路径不受影响）；空配置 = 现状（全部工具），零行为变化。该字段不纳入热加载范围（与"其他字段需重启"的既有姿态一致）。

DataWeave headless 画像示例（config.yaml，**双闸门架构**）：

```yaml
tools:
  default_permission: ""        # 空 = 无规则时走 prompt（仅内置工具会落到这里）
  default_allowed_tools: [Read, Grep, ToolSearch, memory_read, memory_write, session_search, "dataweave__*"]
  preset_rules:
    # 粗闸门：dataweave 工具在 workhorse 侧全放行——细粒度审批语义在平台侧
    - tool: "dataweave__*"
      pattern: "**"
      decision: allow_permanent
    - tool: "Read"
      decision: allow_permanent
    - tool: "Grep"
      decision: allow_permanent
```

职责划分（避免同一动作被问两次）：

- **workhorse = 粗闸门**：`dataweave__*` 全量 `allow_permanent`；permission prompt 只兜内置工具（且 Bash/Write/Edit 已被 `default_allowed_tools` 移出 schema，`registry.Filtered`，`registry.go:103-126`——一切副作用被迫走 DataWeave MCP 工具，R3）。
- **DataWeave PolicyEngine = 细闸门**：危险动作拦截发生在 MCP server 内部。`node_exec`/`publish_workflow`/`delete_*` 由 PolicyEngine 按资源归属判级（L2/L3 → 工具返回 `PENDING_APPROVAL` + 审批单号 → agent 透传审批卡片 → 人批后 agent 调用 `approve_and_execute`）。命令串前缀/管道/重定向解析是 PolicyEngine 的活，workhorse 不需要懂 dw 命令语义。
- workhorse 的 `permission_request`/`permission_resolved` 机制（D3）依然必要：内置工具的审批、`permission_resolved { source: "rule" }` 的审计留痕、以及未叠加平台侧 PolicyEngine 的其他集成方。

### D6 · allowed_tools / default_allowed_tools 条目支持 glob

`Registry.Filtered`（`registry.go:103-126`）当前是精确名集合匹配。改为：条目含 glob 元字符（`*?[`）时按 glob 匹配工具名，否则保持精确匹配（既有调用方语义不变，含 Dispatch 子代理路径）。工具名不含 `/`，glob 行为与权限 tool 字段的 `MatchGlob` 单段语义一致——实现可直接用 `path.Match`，避免 tools → permission 的包依赖（两者对无 `/` 输入等价）。

动机：DataWeave 的 MCP 工具会持续增加，精确名单意味着每加一个工具都要同步改 workhorse 配置，漏改的工具会从 LLM schema **静默消失**（模型根本看不见，极难排查）。`dataweave__*` 一行表达"放行该 server 全部工具"。配套：会话创建时把"已注册但被白名单过滤"的工具名记一条日志（DEBUG 级以上），消除静默消失。

- 备选（引入 `mcp:<server>:*` 专用语法）被否：工具注册名已是 `<server>__<tool>` 单一命名空间，通用 glob 即可表达，无需第二套语法。

## Risks / Trade-offs

- [粗闸门 `dataweave__*` 全放行的前提是平台侧 PolicyEngine 真实承担细粒度审批；若平台策略缺位则危险工具在 workhorse 侧无人拦截] → 这是双闸门架构的有意分工（资源归属判定只有平台做得了）；workhorse 侧保留 `deny_permanent` 叠加能力（deny 优先于任何 allow）作为紧急刹车，spec 场景"tool glob 下 deny 仍优先"固化该机制。
- [移除 loop 预发射事件破坏既有消费者] → 仓库内测试/参考 UI 同步改；`protocol_version` 提升；`permission_resolved` 提供超集信息。
- [MCP server 启动慢拖累 serve 启动] → 连接放 goroutine + 超时，失败仅 WARN 并跳过；工具注册在连接成功后进行（registry.Register 线程安全性需在实现时确认，必要时在启动屏障内完成注册）。
- [tool_use id 作为 request_id 在一次重试中重复] → provider 重试不会复用已发射 tool_use；同一 id 至多一个未决 prompt（loop 串行检查），DataWeave 以最后一次 request 为准。
- [instructions 注入提示词注入面] → instructions 来自 dataweave-api（唯一受信客户端，bearer 鉴权），与现有 AGENTS.md 信任级别一致；长度上限限制滥用。
- [metadata 列新增迁移 v6] → 纯加列 `ALTER TABLE`，无回填；回滚无害（旧版本忽略未知列不成立——sqlite 加列不可逆，但旧代码 SELECT 显式列名不受影响）。

## Migration Plan

1. sqlite v6 迁移（sessions 加 `instructions`、`metadata_json` 列）随二进制升级自动应用。
2. `permission_request` 语义变更随 `protocol_version` 提升发布；DataWeave 桥接层按本文 JSON 契约实现（绿地集成，无旧消费者迁移负担）。
3. 回滚：二进制降级即可；v6 列残留无害。

## Open Questions

（无 —— 审批人身份记录、热加载 MCP 均已明确划入 Non-Goals；如 DataWeave 后续需要 token 轮换，再以独立变更考虑 `auth_header` 的 env 展开。）
