## 1. 项目脚手架

- [ ] 1.1 初始化 Go module `github.com/wallfacers/data-agent`，go.mod 设最低版本 Go 1.22
- [ ] 1.2 建立目录骨架：`cmd/dataagent`、`internal/{api,session,agent,provider,tools,permission,coord,mcp,skills,store,config}`、`pkg/client`、`web`、`test/{e2e,fixtures,mockprovider}`、`scripts`
- [ ] 1.3 添加根级 LICENSE（AGPL-3.0）、README.md（含路径 C 合规声明）、CLAUDE.md
- [ ] 1.4 配置 `.golangci.yml`（启用 govet/staticcheck/errcheck/ineffassign/gosec）
- [ ] 1.5 配置 `.github/workflows/ci.yml`：lint + `go test -race` + multi-arch build matrix（Linux/macOS/Windows × amd64/arm64）
- [ ] 1.6 写 `scripts/build.sh` 用 `-trimpath -ldflags="-s -w"` 出剥离静态二进制

## 2. 配置与启动

- [ ] 2.1 实现 `internal/config`：yaml + env 双源加载；默认值（port=7821、MaxParallelTools=10、max_depth=5、auto_compact_ratio=0.85）
- [ ] 2.2 实现 `cmd/dataagent/main.go`：`init` / `serve` / `version` 三个子命令
- [ ] 2.3 `dataagent init`：交互式生成 `~/.dataagent/{config.yaml,mcp.json,skills/,agents/}` 与空 `state.db`
- [ ] 2.4 `dataagent serve`：装配所有模块、绑 `127.0.0.1:port`、注册 SIGTERM graceful shutdown

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
- [ ] 5.7 实现 `Bash` 工具：`exec.CommandContext` + 进程组、1.5s SIGTERM → SIGKILL；超时配置
- [ ] 5.8 实现 `internal/tools/bash/danger.go` DangerousCommandGuard（8 个正则模式）
- [ ] 5.9 单元测试：5 工具各自的 happy path + workdir 越界 + ctx 取消 + Bash danger 模式命中

## 6. 工具并行编排器

- [ ] 6.1 实现 `internal/agent/orchestrator.go` 的 `batchTools(toolUses []) []ToolBatch` 切批算法
- [ ] 6.2 实现 `runToolBatch`：并发批用 `errgroup + semaphore(MaxParallelTools)`，串行批顺序执行
- [ ] 6.3 实现 ContextModifier 延迟应用（批后顺序 apply）
- [ ] 6.4 实现并发批内单工具失败不取消整批（仅 ctx 取消触发整批停）
- [ ] 6.5 单元测试：表驱动覆盖混合切批、全并发、全串行、单工具 panic、ctx 取消

## 7. 权限模型

- [ ] 7.1 实现 `internal/permission` 的 Permission struct + 匹配器（tool + pattern）
- [ ] 7.2 实现 PermissionStore：scope=permanent 持久化到 SQLite；session/once 仅内存
- [ ] 7.3 实现询问流程：emit `permission_request` → 阻塞 channel 等 `permission_decision` → 5 分钟超时视为 deny
- [ ] 7.4 接入 DangerousCommandGuard：命中即强制询问，绕过 allow 规则
- [ ] 7.5 单元测试：5 种 decision 值的行为、permanent 跨会话生效、deny 阻断、超时

## 8. Session 管理与 Agent 循环

- [ ] 8.1 实现 `internal/session/session.go` Session struct + 状态机 + inbox/outbox channel
- [ ] 8.2 实现 `internal/session/manager.go` SessionManager：CreateSession、GetSession、ListSessions、DeleteSession（含取消级联）
- [ ] 8.3 实现 `internal/agent/loop.go` 主循环：LLM 调用 → 工具批 → 回灌 → 再 LLM
- [ ] 8.4 实现取消时的合成 cancelled tool_result（保留 LLM history 配对完整）
- [ ] 8.5 实现 `internal/agent/compaction.go`：阈值 0.85 触发；用 Fast 模型同家原则总结；保留近 K=8 + 所有 error tool_result
- [ ] 8.6 集成测试：mock provider 跑通"用户提问 → 工具调用 → 回灌 → 文本输出"完整循环
- [ ] 8.7 集成测试：触发压缩 → 验证 history 缩短 + 保留 error + emit compaction 事件
- [ ] 8.8 集成测试：跑中状态 cancel → 验证级联 + 合成 cancelled tool_result + 状态回 Idle

## 9. HTTP + Streamable HTTP API

- [ ] 9.1 实现 `internal/api/server.go` chi router + middleware（recovery、structured logging、optional bearer auth、**Origin 校验**）
- [ ] 9.2 实现 sessions CRUD handlers（POST/GET/DELETE/cancel/compact）
- [ ] 9.3 实现 `internal/api/protocol` 的 ClientEvent / ServerEvent JSON 类型 + 校验（含 `event` 名 → ServerEvent 类型映射）
- [ ] 9.4 实现 `POST /v1/sessions/{id}/stream` handler：JSON 解析 → 校验 type → 入 session.Inbox → 默认返回 `202 Accepted`；未知 type 返回 `400`
- [ ] 9.5 实现 `GET /v1/sessions/{id}/stream` SSE handler：用 std `net/http` Flusher，按 `id: <idx>\nevent: <type>\ndata: <json>\n\n` 格式写；每 25s 写 `: keep-alive\n\n`；并发新 GET 时关旧流并写 `: superseded`
- [ ] 9.6 实现 `Last-Event-ID` header / `?last_event_id=N` query 双路径解析；GET 流先从 events 表回放 `idx > N` 再切实时
- [ ] 9.7 实现 `/health` 与 `/debug/sessions/{id}/events`
- [ ] 9.8 E2E 测试：启动真二进制 + mock LLM → 创建会话 → 浏览器 `EventSource` 接事件 + curl `POST` 发消息 → 多轮对话 → 模拟断线后 EventSource 自动重连验证不漏事件
- [ ] 9.9 E2E 测试：Origin 校验——非白名单 Origin 返 403；白名单通过

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

- [ ] 11.1 实现 `internal/mcp/client.go` JSON-RPC 2.0 客户端（含 initialize、tools/list、tools/call、cancel）
- [ ] 11.2 实现 `internal/mcp/transport_stdio.go`：启子进程、stdin/stdout 行模式、stderr 进日志
- [ ] 11.3 实现 `internal/mcp/transport_http.go`：Streamable HTTP（POST + SSE 配对）
- [ ] 11.4 实现 `internal/mcp/host.go`：生命周期、健康监测、退出后自动重启（最多 3 次）
- [ ] 11.5 实现 `internal/mcp/adapter.go`：MCP tool → 内部 Tool 接口（命名空间 `<server>__<tool>`、保守 IsReadOnly/CanRunInParallel=false）
- [ ] 11.6 集成测试：用一个简单的 echo MCP server（Go 实现）验证 stdio transport + 工具注册 + 调用 + 取消

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
- [ ] 14.3 写 `docs/architecture.md`：模块图与责任清单
- [ ] 14.4 配置 GitHub Actions 的 release workflow：tag 触发 multi-arch binary + checksums
- [ ] 14.5 实际跑一遍端到端：`dataagent init` → `dataagent serve` → curl 创建会话 → wscat 发消息 → 观察事件流

## 15. 上线验证

- [ ] 15.1 跑完整 `go test -race ./...` 必须全绿
- [ ] 15.2 跑 `golangci-lint run` 必须 0 issues
- [ ] 15.3 多平台二进制本地启动验证（Linux amd64 / macOS arm64 至少 2 个平台）
- [ ] 15.4 手工验证 9 个 capability 的所有 Scenario（参照各 spec）
- [ ] 15.5 archive change：`openspec archive init-data-agent-mvp`（将 specs 移到 `openspec/specs/`）
