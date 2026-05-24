## 1. 项目脚手架

- [x] 1.1 初始化 Go module `github.com/wallfacers/data-agent`，go.mod 设最低版本 Go 1.22
- [x] 1.2 建立目录骨架：`cmd/dataagent`、`internal/{api,session,agent,provider,tools,permission,coord,mcp,skills,store,config}`、`pkg/client`、`web`、`test/{e2e,fixtures,mockprovider}`、`scripts`
- [x] 1.3 添加根级 LICENSE（AGPL-3.0）、README.md（含路径 C 合规声明）、CLAUDE.md
- [x] 1.4 配置 `.golangci.yml`（启用 govet/staticcheck/errcheck/ineffassign/gosec）
- [x] 1.5 配置 `.github/workflows/ci.yml`：lint + `go test -race` + multi-arch build matrix（Linux/macOS/Windows × amd64/arm64）
- [x] 1.6 写 `scripts/build.sh` 用 `-trimpath -ldflags="-s -w"` 出剥离静态二进制

## 2. 配置与启动

- [ ] 2.1 实现 `internal/config`：按 `configuration` capability 落地完整 schema；4 源加载（CLI > env > yaml > defaults）；数值与字符串校验（非法配置启动失败 + 明确错误消息）
- [ ] 2.2 实现 `cmd/dataagent/main.go`：`init` / `serve` / `version` 三个子命令
- [ ] 2.3 `dataagent init`：交互式生成 `~/.dataagent/{config.yaml,mcp.json,skills/,agents/}` 与空 `state.db`
- [ ] 2.4 `dataagent serve`：装配所有模块、按 config.server.host:port 绑定、注册 SIGTERM/SIGINT；按 §9.10 graceful shutdown 流程退出
- [ ] 2.5 实现会话并发上限（`sessions.max_concurrent`）：POST `/v1/sessions` 检查活跃 session 数，超限返 429
- [ ] 2.6 实现 history token 硬上限（`agent.max_history_tokens`）：每次 LLM 调用前检查，超限拒绝 user_message

## 3. 持久化层

- [ ] 3.1 引入 `modernc.org/sqlite`，在 `internal/store/sqlite` 实现 Store interface
- [ ] 3.2 编写 migrations：5 张表（sessions、messages、events、tool_calls、permissions）
- [ ] 3.3 实现 Session/Message/Event/ToolCall/Permission 的 CRUD + 增量查询（events `idx > N`）
- [ ] 3.4 单元测试：内存模式（`:memory:`）覆盖所有 CRUD + 增量查询路径

## 4. Provider 抽象与内部 Message

- [ ] 4.1 定义 `internal/provider` 的 Provider interface、Request、ProviderEvent、Message、ContentBlock
- [ ] 4.2 实现 Anthropic adapter：HTTP POST + SSE 解析 + Anthropic Messages → 内部 Message 双向映射
- [ ] 4.3 实现 OpenAI adapter：含 tool_use/tool_result 的特殊翻译（独立 `role:"tool"` 消息、tool_calls 增量累积）
- [ ] 4.4 实现 `ModelPolicy`：Default/Fast/BySessionType + 同家原则（Anthropic→Haiku、OpenAI→gpt-4o-mini）
- [ ] 4.5 实现可重试错误的指数退避（500ms/2s/8s，3 次）+ 不可重试立即终止
- [ ] 4.6 编写 `test/mockprovider`：录制回放 SSE 流，避免测试消耗 token
- [ ] 4.7 集成测试：Anthropic + OpenAI adapter 各自能 stream 一段假响应包括 tool_use

## 5. 工具系统基础

- [ ] 5.1 定义 `internal/tools/tool.go` 的 Tool interface、ToolEnv、ToolResult、ContextModifier
- [ ] 5.2 实现 ToolRegistry：注册/查询/AllowedTools 过滤
- [ ] 5.3 实现 `Read` 工具：含 offset/limit/pages、workdir 路径检查
- [ ] 5.4 实现 `Write` 工具：原子写（temp + rename）、workdir 路径检查
- [ ] 5.5 实现 `Edit` 工具：exact-match 替换、`old_string` 不存在返回 error
- [ ] 5.6 实现 `Grep` 工具：纯 Go regex 实现（不依赖 rg）
- [ ] 5.7 实现 `Bash` 工具：`exec.CommandContext` + `SysProcAttr{Setpgid: true}` 创建进程组；取消时 `syscall.Kill(-pgid, SIGTERM)` → 1.5s 后 SIGKILL 兜底，确保孙进程也被杀；超时配置取 `tools.bash.timeout_seconds`；env 过滤掉危险变量（LD_PRELOAD、LD_LIBRARY_PATH 等）
- [ ] 5.8 实现 `internal/tools/bash/danger.go` DangerousCommandGuard（8 类正则模式 + 已知绕过明示）
- [ ] 5.9 单元测试：5 工具各自的 happy path + workdir 越界 + ctx 取消 + Bash danger 模式命中 + Bash 进程组取消的孙进程清理（`bash -c 'sleep 60 & sleep 60'` 场景）
- [ ] 5.10 单元测试：DangerousCommandGuard 已知绕过测试（hex 转义、绝对路径、bash -c 包装、alias、base64 解码至少各一例，验证 MVP 不防的行为符合 spec）
- [ ] 5.11 实现路径校验工具 `internal/tools/pathguard`：filepath.Clean → EvalSymlinks（含父目录退路）→ filepath.Rel 越界检查 → `O_NOFOLLOW` open（Linux/macOS）/ Lstat 复检（其他平台）。所有读写文件的内置工具与 MCP 工具适配层 SHALL 调用此模块。**来源：AI #2 复审 H-6**
- [ ] 5.12 单元测试：pathguard 拒绝 `..` 穿越、symlink 逃逸、非工作目录路径；TOCTOU 场景（Linux/macOS O_NOFOLLOW）
- [ ] 5.13 实现 `internal/tools/bash/envfilter.go` 集中维护 env 过滤表（精确名 + `DYLD_` 前缀 + `NODE_OPTIONS` 词法 token 前缀判定，用 `shlex` 拆分）；会话级 env 合并时对每 key 重跑过滤、被丢的 key warn 日志（不打 value）。**来源：AI #2 复审 M-6 + Round 3 算法明确化**
- [ ] 5.14 实现 ToolResult.Output 大小自我截断（`tools.tool_result_max_bytes`，默认 1 MiB）：Bash 用 ring buffer / Read 用 limit / Grep 用上限行数；截断追加单行 `[truncated: ...]` 标记；UTF-8 安全回退。**来源：AI #2 复审 tool_result schema 未定义**
- [ ] 5.15 单元测试：Bash env 过滤（启动 env 含 LD_PRELOAD 时子进程无）；ToolResult 截断 5MB 输出回到 1MB + 标记 + UTF-8 边界

## 6. 工具并行编排器

- [ ] 6.1 实现 `internal/agent/orchestrator.go` 的 `batchTools(toolUses []) []ToolBatch` 切批算法
- [ ] 6.2 实现 `runToolBatch`：并发批用 `errgroup + semaphore(MaxParallelTools)`，串行批顺序执行
- [ ] 6.3 实现 ContextModifier 延迟应用（批后顺序 apply）
- [ ] 6.4 实现并发批内单工具失败不取消整批（仅 ctx 取消触发整批停）
- [ ] 6.5 单元测试：表驱动覆盖混合切批、全并发、全串行、单工具 panic、ctx 取消
- [ ] 6.6 实现工具执行全局超时 wrapper：调用 `tool.Run` 前 `context.WithTimeout`；优先级 DefaultTimeout → config.tools.<name>.timeout_seconds → tools.default_timeout_seconds；超时包装为 `{is_error:true, output:"tool execution timed out after Ns"}`。**来源：AI #2 复审 M-7**
- [ ] 6.7 单元测试：超时触发返回正确 tool_result；配置覆盖默认；MCP metadata timeout_seconds 优先

## 7. 权限模型

- [ ] 7.1 实现 `internal/permission` 的 Permission struct + 匹配器（tool exact + pattern glob，基于 `path/filepath.Match` 扩展支持 `**`）
- [ ] 7.2 实现 PermissionStore：scope=permanent 持久化到 SQLite；session/once 仅内存
- [ ] 7.3 实现询问流程：emit `permission_request` → 阻塞 channel 等 `permission_decision` → `agent.permission_request_timeout_seconds` 秒超时视为 deny
- [ ] 7.4 接入 DangerousCommandGuard：命中即强制询问，绕过 allow 规则
- [ ] 7.5 单元测试：5 种 decision 值的行为、permanent 跨会话生效、deny 阻断、超时
- [ ] 7.6 单元测试：glob pattern 匹配（`*` 单段、`**` 多段、字面、`?`）+ Bash/Read/Edit 各自 pattern 语义

## 8. Session 管理与 Agent 循环

- [ ] 8.1 实现 `internal/session/session.go` Session struct + 状态机（6 状态）+ inbox/outbox channel + session 级写锁（保护 outbox 写入与 GET 流切换）
- [ ] 8.2 实现 `internal/session/manager.go` SessionManager：CreateSession、GetSession、ListSessions、DeleteSession（含取消级联）；活跃 session 计数对接 `sessions.max_concurrent`
- [ ] 8.3 实现 `internal/agent/loop.go` 主循环：LLM 调用 → 工具批 → 回灌 → 再 LLM；顶层 `recover()` 包裹（panic → 合成 cancelled tool_result + emit `error{code:"internal_panic"}` + 状态回 Idle，会话可继续）
- [ ] 8.4 实现取消时的合成 cancelled tool_result（output 用 `[CANCELLED] Tool execution was interrupted by user`）；system prompt 中加该前缀语义说明
- [ ] 8.5 实现 `internal/agent/compaction.go`：阈值 `agent.auto_compact_ratio` 触发；用 Fast 模型同家原则总结；保留近 `agent.compact_recent_keep` + 所有 error tool_result；ephemeral session 仅内存压缩
- [ ] 8.6 实现重试逻辑：依赖 ProviderError.IsRetryable() 判断；指数退避按 `agent.provider_retry_backoff_ms`；emit `provider_retry` 事件
- [ ] 8.7 集成测试：mock provider 跑通"用户提问 → 工具调用 → 回灌 → 文本输出"完整循环
- [ ] 8.8 集成测试：触发压缩 → 验证 history 缩短 + 保留 error + emit compaction 事件
- [ ] 8.9 集成测试：跑中状态 cancel → 验证级联 + 合成 cancelled tool_result + 状态回 Idle + 会话可立即接新消息
- [ ] 8.10 集成测试：工具内部 panic → 验证 recover + emit internal_panic + 合成 cancelled + 会话不死 + 其他 session 不受影响
- [ ] 8.11 实现取消收尾超时：`agent.cancel_drain_timeout_seconds`（默认 5s）；超时仍未完成则强制 Idle + emit `error { code: "cancel_timeout", details: { phase, elapsed_ms } }`；卡死的 goroutine 不阻塞会话。**来源：AI #2 复审 H-9**
- [ ] 8.12 集成测试：模拟 MCP 工具不响应 cancel → 5s 后强制 Idle + emit cancel_timeout + session 可立即接新消息

## 9. HTTP + Streamable HTTP API

- [ ] 9.1 实现 `internal/api/server.go` chi router + middleware（recovery、structured logging、Origin 校验、Bearer auth 可选、405 Method Not Allowed for non-GET/POST on /stream）；HTTP server 超时按 `server.*_timeout_seconds` 配置；SSE 端点 WriteTimeout=0
- [ ] 9.2 实现 sessions CRUD handlers（POST/GET/DELETE/cancel/compact）；POST `/v1/sessions` 受 `max_concurrent` 限
- [ ] 9.3 实现 `internal/api/protocol` 的 ClientEvent / ServerEvent JSON 类型 + 校验（含 `event` 名 → ServerEvent 类型映射，11 种事件）
- [ ] 9.4 实现 `POST /v1/sessions/{id}/stream` handler：校验 `Content-Type: application/json`（否则 415）→ JSON 解析 → 校验 type（未知返 400 `unknown_message_type`）→ 状态机检查（冲突返 409 + SSE error 双通道）→ 入 session.Inbox → 默认返回 `202 Accepted`
- [ ] 9.5 实现 `GET /v1/sessions/{id}/stream` SSE handler：校验 `Accept: text/event-stream`（否则 406）；写完整响应 header（Content-Type/Cache-Control/Connection/X-Accel-Buffering）；用 std `net/http` `http.Flusher`，按 `id: <idx>\nevent: <type>\ndata: <compact-json>\n\n` 格式写；JSON 序列化用紧凑模式，含换行的值用 JSON `\n` 转义；每 `sse_keepalive_seconds` 秒写 `: keep-alive\n\n`
- [ ] 9.6 实现 GET 单流切换：session 级写锁内 (1) 旧流写 `: superseded` (2) 关旧 writer (3) 服务新 GET；切换期间事件继续入 events 表，新流通过 Last-Event-ID 回放
- [ ] 9.7 实现 `Last-Event-ID` header / `?last_event_id=N` query 双路径解析；GET 流原子回放算法：写锁内取 `max_idx_snapshot` → 回放 `idx > Last-Event-ID AND idx <= snapshot` → 释放锁 → 切实时通道
- [ ] 9.8 实现 interrupt 到达时清空 outbox 但不删 events 表行
- [ ] 9.9 实现客户端断开检测（`r.Context().Done()`）：停 SSE 写、释放写锁、session goroutine 继续
- [ ] 9.10 实现 Graceful shutdown，按 api-protocol spec "Graceful Shutdown" requirement 的 7 步严格顺序：收 SIGTERM → 停接新连接（**保留已建立 SSE**）→ 触发所有活跃 session 取消（合成 cancelled tool_result + emit `interrupted`）→ 等待 cancelled / interrupted 事件全部写入 events 表与 outbox channel → 所有 SSE emit `error{code:"server_shutdown"}` 并 flush 关闭 → 等 session goroutine 退出（`server.graceful_shutdown_timeout_seconds` 上限，默认 30s，超时强制退出码 1）→ 关 SQLite/MCP host → exit 0。**关键不变量**：cancelled / interrupted 事件 SHALL 先于 server_shutdown 事件送达客户端
- [ ] 9.11 实现 `/health` 与 `/debug/sessions/{id}/events`（debug 端点受 `debug.enabled` 与 bearer auth 双重控制）
- [ ] 9.12 实现 Bearer token 鉴权：constant-time 比较（`crypto/subtle`）；token 永不写日志
- [ ] 9.13 E2E 测试：启动真二进制 + mock LLM → 创建会话 → 浏览器 `EventSource` 接事件 + curl `POST` 发消息 → 多轮对话 → 模拟断线后 EventSource 自动重连验证不漏事件
- [ ] 9.14 E2E 测试：Origin 校验——非白名单/同形异义/null/缺失 origin 各场景；405/406/415 协商
- [ ] 9.15 E2E 测试：POST 到 Compacting/Thinking 等忙状态返 409；SSE 流同时收到 error 事件（双通道一致性）
- [ ] 9.16 E2E 测试：interrupt 后 SSE 不再推积压事件；events 表保留全部；重连按 Last-Event-ID 拉回
- [ ] 9.17 E2E 测试：Last-Event-ID 回放 + 期间新事件无重复无遗漏（race detector 下）
- [ ] 9.18 E2E 测试：GET 单流切换并发安全（race detector + 100 次切换）
- [ ] 9.19 E2E 测试：Bearer auth 在所有端点的行为（含 health 不验、ui 不验、其他都验）
- [ ] 9.20 E2E 测试：SSE event data 含换行的 JSON 编码正确（客户端 `EventSource` onmessage 收到完整对象）
- [ ] 9.21 E2E 测试：Graceful shutdown——SIGTERM 期间活跃 session 客户端**先**收到 cancelled tool_result 与 `interrupted` 事件，**后**收到 `error{code:"server_shutdown"}`，最后连接关闭；ephemeral session 同样收到完整 cancelled/interrupted（验证 spec "Ephemeral session 取消事件不丢" Scenario）；进程在 timeout 内退出码 0
- [ ] 9.22 集成测试：nginx 反代 + `proxy_buffering off` 场景下 SSE 流正常推送（参考 `docs/deployment.md`）
- [ ] 9.23 实现 POST body 大小限制：所有 POST 端点用 `http.MaxBytesReader` 包裹；超 `server.max_request_body_bytes`（默认 1 MiB）返 `413` 含 `{code:"request_too_large", limit}`。**来源：AI #2 复审 M-3**
- [ ] 9.24 单元/集成测试：POST 1 MiB 通过、5 MiB 拒；配置覆盖（设 512 KiB 时在该阈值处拒）
- [ ] 9.25 实现 `error` 事件完整 JSON schema 与 code 枚举（含 14 种 code、recoverable 标志、details 子字段）；event 序列化器与单元测试。**来源：AI #2 复审 M-10**
- [ ] 9.26 单元测试：14 种 error code 各自的 details 结构正确；recoverable 标志符合 spec 表格

## 10. 多 agent 协作

- [ ] 10.1 实现 `internal/coord/agenttype.go` 加载 `~/.dataagent/agents/*.yaml`（启动 + Dispatch 调用前热扫描）
- [ ] 10.2 实现 `internal/tools/dispatch/tool.go` Dispatch 工具（CanRunInParallel=true）
- [ ] 10.3 实现子 session 创建：parent_id 外键、继承 workdir/env/provider/model（参数可覆盖）、独立 history
- [ ] 10.4 实现 max_depth 上限（默认 5）拒绝过深 Dispatch
- [ ] 10.5 实现事件透传：streaming 模式包装为 subagent_event；blocking 模式不透传
- [ ] 10.6 实现取消级联：父 ctx 取消 → 所有子 ctx 取消
- [ ] 10.7 实现错误隔离：子 panic / 错误 → 父看到 tool_result.is_error=true
- [ ] 10.8 集成测试：父一轮并发 dispatch 3 个子 → 验证并发执行 + 各自独立 history + 事件透传

## 11. MCP 客户端

- [ ] 11.1 实现 `internal/mcp/client.go` JSON-RPC 2.0 客户端（含 initialize 握手、tools/list、tools/call、notifications/cancelled）
- [ ] 11.2 实现 `internal/mcp/transport_stdio.go`：启子进程、stdin/stdout 行模式、stderr 进日志
- [ ] 11.3 实现 `internal/mcp/transport_http.go`：Streamable HTTP（POST endpoint + GET SSE 长连接 + Last-Event-ID 重连，1s/3s/10s/30s 退避）
- [ ] 11.4 实现 `internal/mcp/host.go`：生命周期与 session 解耦（进程级共享）；健康监测；stdio server 退出后自动重启（最多 3 次）；进程退出时 graceful shutdown 所有 server
- [ ] 11.5 实现 `internal/mcp/adapter.go`：MCP tool → 内部 Tool 接口（命名空间 `<server>__<tool>`、保守 IsReadOnly/CanRunInParallel=false 除非 metadata 声明）
- [ ] 11.6 集成测试：用一个简单的 echo MCP server（Go 实现）验证 stdio transport + 工具注册 + 调用 + 取消
- [ ] 11.7 集成测试：HTTP transport 验证 initialize 握手取回 Mcp-Session-Id；POST/GET 流共用 endpoint
- [ ] 11.8 集成测试：HTTP MCP client SSE 断线自动重连（mock server 主动断开，验证客户端按退避重连）

## 12. Skills 加载器

- [ ] 12.1 实现 `internal/skills/loader.go`：扫描 `~/.dataagent/skills/*/skill.yaml`、处理同名冲突、文件缺失跳过
- [ ] 12.2 实现 `internal/skills/injector.go`：把 skill 清单注入 system prompt（`<available_skills>` 块）
- [ ] 12.3 实现 `internal/skills/loadtool.go` LoadSkill 工具（CanRunInParallel=true）
- [ ] 12.4 实现 LoadSkill 触发的 AllowedTools 子集应用（直到会话结束或被另一个 LoadSkill 覆盖）
- [ ] 12.5 集成测试：放 2 个 skill yaml → 创建会话 → 验证 system prompt 含清单 → LoadSkill → tool_result 含内容

## 13. 参考 Web UI

- [ ] 13.1 写 `web/index.html` + `web/app.js`（~200 行原生 JS）：会话列表、新建、`EventSource` 接事件流、`fetch POST` 发消息、消息渲染、工具调用展示、权限询问对话框
- [ ] 13.2 `//go:embed` 把 web/ 编进二进制，挂在 `/ui`
- [ ] 13.3 文档化协议供自定义 UI 作者参考：`docs/protocol.md`

## 14. 文档与发布

- [ ] 14.1 写 README.md：合规声明 + 快速开始 + 配置说明 + provider 兼容范围声明
- [ ] 14.2 写 `docs/protocol.md`：HTTP REST + Streamable HTTP（POST + GET SSE + Last-Event-ID）完整协议规范，含与 MCP 2025-11-25 spec 对应关系说明
- [ ] 14.2.1 写 `docs/deployment.md`：本地启动、配置文件位置、nginx 反代示例（`proxy_buffering off` + `proxy_read_timeout 3600s` + `Origin` 透传）、systemd unit 模板、Bearer auth 启用步骤。**来源：AI #1 复审 2026-05-24（tasks 9.22 已引用此文件但缺创建任务）**
- [ ] 14.3 写 `docs/architecture.md`：模块图与责任清单
- [ ] 14.4 配置 GitHub Actions 的 release workflow：tag 触发 multi-arch binary + checksums
- [ ] 14.5 实际跑一遍端到端：`dataagent init` → `dataagent serve` → curl 创建会话 → `curl -X POST .../stream` 发 user_message → `curl -N .../stream` 观察 SSE 事件流

## 15. 上线验证

- [ ] 15.1 跑完整 `go test -race ./...` 必须全绿
- [ ] 15.2 跑 `golangci-lint run` 必须 0 issues
- [ ] 15.3 多平台二进制本地启动验证（Linux amd64 / macOS arm64 至少 2 个平台）
- [ ] 15.4 手工验证 **10 个 capability** 的所有 Scenario（参照各 spec）
- [ ] 15.5 长期运行测试：创建/销毁 100 个 session 后 `runtime.NumGoroutine()` 回到基线（goroutine 泄漏检测）
- [ ] 15.6 archive change：`openspec archive init-data-agent-mvp`（将 specs 移到 `openspec/specs/`）
