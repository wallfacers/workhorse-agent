## Context

`data-agent` 是从零开始的本地 AI agent 服务端。当前没有现成的 Go 实现能同时满足：多轮对话 + 并行工具 + 多 agent 并发派发 + MCP + Skills + 全双工 WebSocket 协议 + 自定义 UI 解耦。OpenCode 是 TypeScript 写的并以 SSE 半双工为主；Claude Code 是 TypeScript CLI、闭源且无 server 模式。空白处需要填补。

技术参考路径取 **路径 C：参考架构再实现** ——可以通过 `claude-code-sourcemap`（npm 公开包附带的 source map 还原版本）理解架构思想，但代码、命名、目录结构、协议字段、字符串内容全部独立设计。

## Goals / Non-Goals

**Goals:**

- 单 Go 二进制启动即可服务，无外部依赖
- 支持单用户并发多会话，每会话独立 workdir / env / history / 取消上下文
- WebSocket 全双工事件流；客户端断线不中断 LLM 推理；重连按 `since_event_idx` 增量同步
- 工具批量并行执行；并发批内单工具失败不取消整批；上下文修改延迟应用
- 父 agent 通过 Dispatch 工具一轮并发派发多个子 session，每个子 session 是完整独立的 agent 循环
- Provider 抽象支持 Anthropic Messages + OpenAI Chat Completions；可接 OpenAI 兼容端
- 持久化基于 SQLite append-only 事件流；客户端可重放任意历史
- AGPL-3.0，确保下游修改必须开源

**Non-Goals:**

- Hooks 机制（V2）
- ACP 协议兼容（V2；事件结构留扩展）
- 多租户 / OAuth / 完整鉴权（仅单 bearer token）
- 容器 / bubblewrap 沙箱（仅 workdir 路径检查）
- Web UI 美化（极简能跑即可）
- Prometheus / OpenTelemetry（V2）
- Voice / 多模态 / 远程会话

## Decisions

### D1 · 语言选择：Go

选 Go 而非 Rust / Elixir：
- **vs Rust**：goroutine 是天然的多 agent 基元，开发迭代速度比 Rust 快 2-3 倍；MVP 6-9 周可交付而非 6 个月；性能差距（20-40%）对 LLM-bound 工作负载无意义（瓶颈在 token 流）
- **vs Elixir**：Erlang VM 适合 actor 但缺 AI 生态；HTTP 调 LLM 不需要 BEAM 的弹性优势；Go 单二进制分发更适合 CLI 工具型产品

### D2 · 传输协议：WebSocket 全双工

选 WebSocket 而非 SSE / gRPC bidi / MCP-style Streamable HTTP：
- **vs SSE**：SSE 是单向（server→client），用户取消、人工接管、追加上下文等控制信号需要单独 POST endpoint 配对，半双工本质
- **vs gRPC bidi**：HTTP/2 流真双向但浏览器原生 fetch 不支持 streamed request；自定义 Web UI 接入复杂
- **vs MCP Streamable HTTP**：POST + SSE 配对在语义上接近全双工但不是；MCP 自己用得着是因为协议特性，应用层全双工 WebSocket 更直接
- WebSocket 单连接、所有客户端语言都有库、Go 端 `nhooyr.io/websocket` 简洁现代

### D3 · 部署形态：本地单进程 + 多会话隔离

选这个而非多租户云端：
- 用户群是单用户开发者，类似 Claude Code / OpenCode 的部署模型
- 多租户云端是 V2+ 课题，需要鉴权、配额、审计、文件隔离，工程量翻倍
- 架构留出 `user_id` / `workspace_id` 抽象位置，将来上多租户不重写

### D4 · 持久化：SQLite + 事件日志 (append-only)

选 SQLite 而非 Postgres / 内存：
- 本地单进程不需要 server-style DB
- `modernc.org/sqlite` 纯 Go，无 CGO，编译跨平台静态二进制
- 事件流 append-only 是"自定义 UI"和"断线重连"的共同基础——客户端可重放任意 `since_event_idx` 之后的事件

### D5 · 并行工具执行：按 `CanRunInParallel` 分批

参考 Claude Code 的 `partitionToolCalls` 思路（仅架构层面），具体实现独立：
- LLM 一轮返回的 `tool_use[]` 按 `Tool.CanRunInParallel(input)` 切批：连续可并发的合成一批
- 批内 `errgroup` + `semaphore(MaxParallelTools=10)` 并发执行；批与批之间顺序
- 单工具失败 ≠ 整批取消；ctx 取消 = 全批一起停
- 上下文修改延迟应用：并发批内的 ContextModifier 排队，批完成后顺序 apply，避免 ToolEnv 并发写竞争
- Dispatch 工具 `CanRunInParallel=true` —— 这是"小龙虾模式"的开关，父 agent 一轮可并发派多个子 agent

### D6 · Provider 抽象：Anthropic 语义为模板

内部 Message 格式取 Anthropic Messages 语义（`role + blocks[]`，blocks 包含 `text / tool_use / tool_result`）：
- Anthropic adapter 几乎 1:1，最轻
- OpenAI adapter 处理差异：`tool_use` block ↔ `assistant.tool_calls[]`；`tool_result` block ↔ 独立 `role:"tool"` 消息；文本与 tool_use 不能交错的强制规则
- 不引入 `anthropic-sdk-go` / `openai-go`，自写薄 HTTP 客户端（SSE 解析 + POST），合规留痕 + 减少依赖锁定

### D7 · 多 agent 协作：父子 session 同构

主 agent 与子 agent 是同一种 Session struct，差别在 `parent_id` 外键 + 触发来源：
- 子 session 完全独立的 history + goroutine
- 默认继承父的 workdir / env / provider / model（可在 agent_type.yaml 或 Dispatch 参数覆盖）
- MCP host 共享（避免每子重启 MCP server）
- 取消级联：父 ctx → 所有跑中的子 ctx
- 事件透传默认 `streaming` —— 多 agent 协作的核心价值是可观察性

### D8 · 取消语义：半完成 tool_result 合成 cancelled 标记

取消时正在跑的工具被砍：
- 已完成的 tool_result：保留入 history
- 未完成的 tool_result：写入合成 `{is_error: true, output: "<cancelled by user>"}`
- 这样 LLM 下一轮看到完整 tool_use ↔ tool_result 配对，不会困惑或重试

### D9 · 上下文压缩：阈值 0.85 + 同家 Fast 模型

- 触发：token 用量 > 模型上下文窗口 × 0.85，或用户显式调
- 保留最近 K=8 条原始 + 所有 `is_error=true` 的 tool_result（重要错误信号）
- 用 Fast 模型总结（**同家原则**：Anthropic session → Haiku；OpenAI session → `gpt-4o-mini`），避免跨家风格不一致
- 压缩有损但 messages 表保留原始记录，UI 可看完整对话

### D10 · Bash 安全：仅"重大隐患"防护

MVP 不做命令分类（白名单 + 解析）。仅维护小型 DangerousCommandGuard 正则列表：`rm -rf /`、`rm -rf ~`、`dd of=/dev/`、`mkfs.*`、重定向到块设备、fork bomb、`chmod -R 777 /`、`shutdown/reboot/halt/poweroff`。命中即**强制询问**，绕过任何 `allow_permanent` 规则；事件标 `risk: "catastrophic"`，UI 应红色高亮。

### D11 · 合规：路径 C 操作规则

- 不复制代码：阅读 sourcemap 后关闭文件，用 Go 抽象重写
- 不复制字符串：system prompt、错误消息、事件名、文案全部独立
- 不复制目录/文件名：`internal/tools/readfile/` ≠ `src/tools/FileReadTool/`
- 不复制 magic number：阈值、超时、并发上限独立校准
- Git 提交从第一行代码开始，独立工程记录
- README 顶部声明研究性质 + 非商业用途 + 无侵权意图
- AGPL-3.0

## Risks / Trade-offs

| 风险 | 缓解 |
|---|---|
| WebSocket 全双工对老 HTTP 代理 / 企业防火墙不够友好 | 端口默认 `127.0.0.1:7821`，本地用不受代理影响；未来需要时加 fallback HTTP polling |
| `modernc.org/sqlite` 是 transpiled C，比 mattn/go-sqlite3 慢 2-3 倍 | 本地单进程 IOPS 量极低（每事件 1 行 insert），跑得动；换 mattn 需要 CGO，破坏静态二进制目标 |
| 工具并行执行竞争资源（CPU/IO/文件锁） | semaphore 默认 10 上限；Edit/Write 类工具 `CanRunInParallel=false` 强制串行；用户可配 |
| 法律边界主观判断 | 路径 C 操作规则（D11）形成"防御性证据链"；Git 历史从空仓库起步可追溯；如必要时可再走"清洁室"流程把 sourcemap 隔离到不同人员 |
| Bash 危险命令防护清单不全 | MVP 接受这个风险（用户对自己机器负责）；社区可 PR 补充；V2 加可插拔规则 |
| OpenAI Chat Completions 与 Anthropic Messages tool use 语义差异 | adapter 层负责翻译；某些 Anthropic 独有能力（thinking、prompt cache）只在 Anthropic provider 启用 |
| LLM 输出 tool_use 时不可控的"幻觉调用"（指向不存在的工具） | 工具注册表 lookup 失败 → emit `error` 事件 → 当前轮终止，错误回灌 LLM 让它重试 |
| 子 agent 嵌套深度无限制可能 OOM / 死循环 | session struct 含 `depth` 字段，超过 `max_depth=5`（默认）拒绝再 Dispatch；可配 |
| 上下文压缩有损可能丢关键信息 | 保留所有 `is_error=true` 的 tool_result；messages 表保留原始记录可恢复；用户可显式禁用自动压缩 |

## Migration Plan

不涉及（项目从零开始）。

部署：
1. `go build` 出二进制
2. 用户跑 `dataagent init` 生成 `~/.dataagent/config.yaml`（API key、provider、端口）
3. `dataagent serve` 启动
4. 自定义 UI 连 `ws://127.0.0.1:7821/v1/sessions/{id}/stream`

回滚：本地服务，删二进制 + `~/.dataagent/` 即清空。

## Open Questions

- **Skills 配置文件格式**：当前定 YAML（`skill.yaml`）以跟原版 Markdown frontmatter 形成结构差异（路径 C 合规）。若实际编写体验差，可在实施阶段切回 Markdown + frontmatter（解析器替换成本低）
- **`MaxParallelTools` 默认值 10**：实际跑下来可能太激进（CPU / 文件 IO 抖动），实施后压测调整
- **断线重连 events 表大小上限**：默认全量保留；超大 session 可能让重连慢；MVP 不做截断，V2 加 `events_retention_count` 配置
- **多 agent 深度上限 `max_depth=5`**：经验值，需观察实际使用模式调整
