## Why

当前没有 Go 实现的本地 AI agent 服务器：能多轮对话、并行工具执行、并发派发子 agent、接 MCP、加载 Skills，对外以 **MCP Streamable HTTP**（POST 提交 + GET SSE 接收，符合 MCP 2025-11-25 spec）暴露统一传输层供任意客户端（CLI / Web UI / IDE 扩展）接入。选这个传输是为了与项目内部的 MCP 客户端协议层统一、浏览器/curl 无需特殊握手即可调、利用 SSE `Last-Event-ID` 标准头自然实现断线重连。这一版要补齐这个空白，以 Anthropic Claude Code 的架构为参考思想（不复制代码、命名、协议字段），用 Go 重新设计与实现，定位本地单用户多会话、可商用研究项目。

## What Changes

- 新建 Go 单进程服务 `dataagent`，绑 `127.0.0.1`，单二进制分发
- HTTP REST 提供会话 CRUD、取消、压缩、健康检查
- MCP Streamable HTTP 传输层提供事件流：POST `/v1/sessions/{id}/stream` 提交客户端消息（5 种，默认 `202 Accepted`），GET `/v1/sessions/{id}/stream` 开启长连接 SSE 接收服务端事件（10 种），每个 SSE event 含 `id:` 字段（值为单调递增 `idx`），断线重连用标准 `Last-Event-ID` HTTP header 增量同步
- 内置 5 个工具：Read / Write / Edit / Bash / Grep
- 工具并行执行：按 `CanRunInParallel` 把同轮 tool_use 分批，并发批内 `errgroup + semaphore`
- Bash 危险命令防护：仅对 `rm -rf /`、`dd of=/dev/`、`mkfs`、fork bomb、`shutdown` 等模式强制询问，绕过任何 permanent 允许规则
- Provider 抽象：官方支持 Anthropic Messages + OpenAI Chat Completions；OpenAI-兼容国内模型可接但不维护
- 多 agent 协作：Dispatch 工具 `CanRunInParallel=true`，父 session 可一轮启动多个子 session 并发执行
- 上下文自动压缩：阈值 0.85；保留近 K=8 条 + 所有 `is_error` 的 tool_result；用 Fast 模型（同家原则）
- 会话持久化：SQLite（modernc.org/sqlite，纯 Go）；events 表 append-only，支持断线重连按 SSE 标准 `Last-Event-ID` HTTP header（或备用 `?last_event_id=N` query）增量拉取
- 会话隔离：每 session 独立 workdir / env / history；取消级联到工具、子进程、子 session
- 权限模型：`allow_once / allow_session / allow_permanent / deny / deny_permanent`，匹配 tool+pattern
- MCP 客户端：stdio + Streamable HTTP；MCP tool 适配进内部 Tool 注册表
- Skills 加载器：扫 `~/.dataagent/skills/*/skill.yaml`；trigger 注入 system prompt；LoadSkill 工具按需加载内容
- 极简参考 Web UI：用 `//go:embed` 内嵌单页 HTML，鼓励自定义客户端

## Capabilities

### New Capabilities

- `api-protocol`: HTTP REST 端点 + MCP Streamable HTTP 传输（POST + GET SSE）+ Origin 校验 + 事件日志格式 + `Last-Event-ID` 断线重连
- `session-management`: 会话 CRUD、持久化、状态机（Idle/Thinking/AwaitPerm/Executing/Compacting/Cancelled）、隔离（workdir/env/history）
- `agent-loop`: LLM 调用循环、消息结构、上下文压缩触发与执行
- `tool-system`: Tool 接口、并行执行编排器、5 个内置工具
- `provider-abstraction`: Provider 接口、Anthropic adapter、OpenAI adapter、内部统一 Message 格式、模型选择策略
- `permission-control`: 权限规则匹配、询问/允许/拒绝流程、Bash 危险命令防护
- `multi-agent`: Dispatch 工具、子 session 隔离、Agent 角色配置（yaml 热加载）、事件透传（streaming / blocking）
- `mcp-integration`: stdio / Streamable HTTP transport、JSON-RPC 客户端、MCP tool 适配
- `skills-loader`: skill 发现与 frontmatter 解析、按需注入 system prompt、LoadSkill 工具

### Modified Capabilities

无（项目从零开始，无既有 spec）

## Impact

- **新增代码**：8,000-12,000 行 Go（不含测试）
- **新增依赖**：~9 个直接依赖（chi、modernc.org/sqlite、yaml.v3、jsonschema、errgroup/semaphore、slog、ulid、shlex；SSE/HTTP 用 std `net/http`，不引入 WebSocket 库）
- **新增配置目录**：`~/.dataagent/{config.yaml, state.db, mcp.json, skills/, agents/}`（首次启动自动创建）
- **运行时**：本地服务，监听 `127.0.0.1:7821`（默认），可选 bearer token 鉴权
- **二进制**：12-18MB 静态二进制，多平台 matrix（Linux/macOS/Windows × amd64/arm64）
- **License**：AGPL-3.0
- **法律边界**：路径 C（参考架构再实现），不复制源码/命名/字符串/目录结构；README 顶部声明研究性质
