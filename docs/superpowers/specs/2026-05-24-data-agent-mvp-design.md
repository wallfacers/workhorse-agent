# data-agent · MVP 设计文档

- **日期**：2026-05-24
- **作者**：wallfacers
- **状态**：Draft（待 spec 复审）
- **预估范围**：单人全职 6-9 周；核心代码 8,000-12,000 行 Go

---

## 1. 摘要

**data-agent** 是一个用 Go 编写的本地、单进程、多会话的 AI agent 服务。它对外暴露 HTTP + WebSocket 接口，支持多轮对话、并行工具执行、多 agent 协作（"小龙虾"模式：父 agent 并发派发子 agent）、MCP 客户端、Skills 加载器。

第一版（MVP）目标：跑通端到端多轮对话 + 5 个内置工具 + 子 agent dispatch + MCP + Skills + Anthropic / OpenAI 双 provider。

设计上参考 Anthropic Claude Code 的架构思想（通过其 npm 公开包附带的 source map 还原版本），但所有代码、命名、目录结构、协议字段、字符串内容均独立设计，符合"参考架构再实现"的合规路径（详见 §4）。

---

## 2. 项目定位

| 维度 | 选择 |
|---|---|
| 语言 | Go |
| 部署形态 | 本地单进程 + 多会话隔离（单用户） |
| 协议 | HTTP REST（CRUD）+ WebSocket（全双工事件流） |
| 持久化 | SQLite（modernc.org/sqlite，纯 Go） |
| LLM Provider | Anthropic Messages + OpenAI Chat Completions（其他厂商自行适配 base_url） |
| 多 agent 模式 | 父 session 通过 Dispatch 工具并发启动子 session |
| UI | 服务仅暴露协议；内置极简参考 Web UI，鼓励自定义客户端 |
| License | AGPL-3.0 |

非目标参见 §15。

---

## 3. 第一版（MVP）范围

包含：

- 会话管理（create / list / delete / cancel）
- 多轮对话 + 流式输出（token-by-token）
- 5 个内置工具：Read / Write / Edit / Bash / Grep
- 并行工具执行（按"是否可并发"分批）
- 上下文自动压缩（auto-compaction）
- 会话持久化（SQLite，含事件日志可重放）
- 取消 / 中断机制（级联到工具、子进程、子 agent）
- 动态工具发现（每会话可拥有不同工具集）
- 子 agent 调度（Dispatch 工具 + 并发执行 + 独立 history）
- MCP 客户端（stdio + Streamable HTTP）
- Skills 加载器（按需注入 + LoadSkill 工具）
- 权限模型（allow / deny / ask；session 与永久作用域）
- 断线重连（基于事件日志增量同步）
- 极简参考 Web UI（嵌入二进制）

不包含（V2+）：参见 §15。

---

## 4. 合规与法律框架

本项目采用"**参考架构再实现**"（清单见 §4.2），具体含义：

### 4.1 允许做什么

- 阅读 claude-code-sourcemap 还原代码以理解架构思想（如"工具按并发性分批"、"父子 session 模型"等）
- 实现公开协议（MCP、Anthropic Messages API、OpenAI Chat Completions API、HTTP / WebSocket）

### 4.2 不允许做什么

| 项 | 做法 |
|---|---|
| **代码翻译** | claude-code-sourcemap 的 `.ts/.tsx` 文件不直接翻译成 Go。阅读后必须关闭文件，用自己的语言和抽象重写 |
| **字符串复制** | 不抄系统 prompt、错误消息、事件名常量、用户可见文案 |
| **目录/文件结构复制** | 我们的 `internal/tools/readfile/` ≠ 原版 `src/tools/FileReadTool/`，结构与命名风格独立 |
| **命名约定复制** | Go 风格：包名小写 + 类型 PascalCase + 函数 camelCase；接口命名（如 `IsReadOnly`）从 Go 习惯自然得出，而非机械大小写转换 |
| **Magic number 复制** | token 阈值、超时值、并发上限等独立校准 |
| **System prompt 复制** | 自己写 system prompt，措辞独立 |
| **Skills 内容复制** | 仅提供加载器框架，skill 内容由用户自行编写 |
| **商标使用** | README / UI 不出现 "Claude Code" 字样作为产品宣传 |

### 4.3 工程留痕

- Git 提交从第一行代码开始（独立工程记录）
- 每个 PR 描述写明"独立实现的部分 vs 参考的架构思想"
- 项目 README 顶部声明：本项目独立设计与实现，仅在架构思想层面参考 Anthropic Claude Code（通过公开 npm 包附带的 source map）。所有代码、协议、命名独立设计。不用于商业用途，无侵权意图，仅供技术研究。

### 4.4 License

AGPL-3.0：要求修改后必须开源，阻止商业 fork 闭源使用。

---

## 5. 总体架构

```
┌───────────────────────────────────────────────────────────────────┐
│                       dataagent (Go binary)                        │
│                                                                    │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │  api/  HTTP server (chi router)                            │  │
│  │  ├─ POST   /v1/sessions          创建会话                  │  │
│  │  ├─ GET    /v1/sessions          列表                      │  │
│  │  ├─ DELETE /v1/sessions/{id}     销毁                      │  │
│  │  ├─ POST   /v1/sessions/{id}/cancel  中断当前推理          │  │
│  │  └─ GET    /v1/sessions/{id}/stream  ◄── WebSocket 升级    │  │
│  └────────────────────┬───────────────────────────────────────┘  │
│                       │                                            │
│  ┌────────────────────▼───────────────────────────────────────┐  │
│  │  session/   Session Manager + 每会话状态机                  │  │
│  └────────────────────┬───────────────────────────────────────┘  │
│                       │                                            │
│  ┌──────┬─────────┬───▼────┬──────────┬──────────┬─────────┐    │
│  │agent │provider │ tools  │   mcp    │  skills  │  store  │    │
│  │ loop │ pool    │registry│  client  │  loader  │ SQLite  │    │
│  └──────┴─────────┴────────┴──────────┴──────────┴─────────┘    │
└───────────────────────────────────────────────────────────────────┘
       ▲                                              ▲
       │ WebSocket                                    │ WebSocket
       └─── 参考 Web UI (内嵌, /ui)                   └── 自定义客户端 (任意语言)
```

### 5.1 模块职责（一句话）

| 模块 | 职责 | 不做 |
|---|---|---|
| `api/` | HTTP + WebSocket 收发；JSON 编解码；鉴权 | 不做业务逻辑 |
| `session/` | 会话状态机；inbox/outbox；持久化触发 | 不直接调 LLM/工具 |
| `agent/` | LLM 循环（system+history+tools→response→tool_use→tool_result→...）；上下文压缩 | 不知道传输层 |
| `provider/` | Anthropic + OpenAI adapter；输入输出统一 `Message` 类型 | 不缓存 |
| `tools/` | 工具注册表 + 内置 5 工具；每个工具实现 `Tool` 接口 | 不调 LLM |
| `permission/` | 工具调用前的允许/询问/拒绝决策；规则匹配 | 不执行工具 |
| `coord/` | 子 agent dispatch；事件转发；结果收集；agent 角色配置 | 不做长期记忆 |
| `mcp/` | 管理 stdio / HTTP MCP 子进程；适配 MCP 工具到内部接口 | 不实现具体工具 |
| `skills/` | 扫描目录、解析 frontmatter、按需注入 system prompt；LoadSkill 工具 | 不执行 skill 内容 |
| `store/` | SQLite 持久化 sessions/messages/events；纯 Go 驱动 | 不缓存到内存 |
| `config/` | 配置加载（yaml + env） | - |

---

## 6. 协议设计

### 6.1 HTTP REST

| Method | Path | 描述 |
|---|---|---|
| `POST` | `/v1/sessions` | 创建会话；body 含 workdir、env、initial system addons |
| `GET` | `/v1/sessions` | 列表 |
| `GET` | `/v1/sessions/{id}` | 详情 |
| `DELETE` | `/v1/sessions/{id}` | 销毁 |
| `POST` | `/v1/sessions/{id}/cancel` | 中断当前推理 |
| `POST` | `/v1/sessions/{id}/compact` | 手动触发上下文压缩 |
| `GET` | `/v1/sessions/{id}/stream` | WebSocket 升级 |
| `GET` | `/debug/sessions/{id}/events?since=N` | 事件流回放（DEBUG 模式） |
| `GET` | `/health` | 健康检查 |
| `GET` | `/ui` | 嵌入式参考 Web UI |

### 6.2 WebSocket 消息

WebSocket 上每条消息是一个 JSON 对象。

**Client → Server**（5 种）：

```jsonc
{ "type": "user_message", "content": "...", "attachments": [...] }
{ "type": "permission_decision", "request_id": "...", "decision": "allow_once|allow_session|allow_permanent|deny|deny_permanent" }
{ "type": "interrupt" }
{ "type": "ping" }
{ "type": "context_update", "workdir": "...", "files": [...] }
```

**Server → Client**（10 种）：

```jsonc
{ "type": "assistant_text_delta", "delta": "..." }
{ "type": "assistant_text_done", "message_id": "..." }
{ "type": "tool_call_start", "id": "...", "tool": "Bash", "input": {...} }
{ "type": "tool_call_done", "id": "...", "output": "...", "ok": true, "took_ms": 42 }
{ "type": "permission_request", "request_id": "...", "tool": "Bash", "preview": "rm -rf /", "risk": "catastrophic" }
{ "type": "subagent_event", "agent_id": "...", "event": {...} }
{ "type": "compaction", "before": 12000, "after": 3500 }
{ "type": "error", "code": "...", "message": "..." }
{ "type": "interrupted" }
{ "type": "pong" }
```

### 6.3 协议设计原则

- 所有事件可序列化、可重放 → 存 SQLite 事件流 → 自定义 UI 可"快进回放"
- `message_id` / `request_id` / `session_id` / `agent_id` 全部 ULID（时间有序、不可猜）
- 事件名独立设计，不复用 Anthropic 内部事件名
- 不引入 ACP 协议；事件结构留扩展空间，未来要兼容 ACP 时加 adapter 即可

详细协议见 `docs/protocol.md`（实现阶段产出）。

---

## 7. 数据流 + 会话生命周期 + 持久化

### 7.1 一次"用户问→助手答"完整流程

```
User                  api/ws         session #1         agent loop        provider     tools
 │                      │                │                  │                │           │
 │─user_message────────▶│                │                  │                │           │
 │                      │──Inbox<-msg───▶│                  │                │           │
 │                      │                │──run iter───────▶│                │           │
 │                      │                │                  │──Messages.Create─▶│         │
 │                      │                │                  │◀─stream chunks──│         │
 │◀─assistant_text_delta│◀─Outbox<-evt──│◀─emit───────────│                │           │
 │                      │                │                  │  (LLM 决定调 Bash) │           │
 │                      │                │                  │──prepare call──┐│           │
 │                      │                │                  │ permission ◀───┘│           │
 │◀─permission_request──│◀──────────────│◀────────────────│                │           │
 │─permission_decision─▶│                │                  │                │           │
 │                      │──Inbox<-dec──▶│──forward──────▶ │──exec─────────▶ ─Bash───│
 │                      │                │                  │◀─output─────────│◀──ok──│
 │◀─tool_call_done──────│◀─emit─────────│◀────────────────│                │           │
 │                      │                │                  │──next iter:LLM─▶│         │
 │                      │                │                  │◀──final text───│           │
 │◀─assistant_text_done─│◀──────────────│◀────────────────│                │           │
```

### 7.2 Session 状态机

```
       ┌──────────┐
       │  Idle    │◄────────── 创建 / 上轮完成
       └────┬─────┘
            │ user_message 入 inbox
            ▼
       ┌──────────┐
       │ Thinking │ ── LLM 调用中（可被 interrupt → Cancelled）
       └────┬─────┘
            │ tool_use 出现
            ▼
       ┌──────────┐
       │AwaitPerm │ ── 等用户/规则决策
       └────┬─────┘
            │ allow
            ▼
       ┌──────────┐
       │Executing │ ── 工具执行中
       └────┬─────┘
            │ tool_result 回灌
            ▼
       ┌──────────┐
       │ Thinking │ ── 是否还有更多 tool_use？没有就回 Idle
       └──────────┘

中间任意状态：interrupt → Cancelled → 收尾 → Idle
压缩触发：任何 Thinking 之后 → Compacting → Idle
```

### 7.3 Session struct（核心字段）

```go
type Session struct {
    ID       ULID
    Workdir  string
    Env      map[string]string
    Provider string                  // "anthropic" | "openai"
    Model    string
    Inbox    chan ClientEvent
    Outbox   chan ServerEvent
    State    atomic.Value
    history  []Message
    cancel   context.CancelFunc
    parent   *ULID                   // 子 session 的父 ID（顶层 session 为 nil）
}

func (s *Session) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done(): return
        case ev := <-s.Inbox:
            s.handle(ctx, ev)
        }
    }
}
```

### 7.4 持久化（SQLite）

5 张表：

```sql
sessions(id PK, parent_id FK, workdir, provider, model, created_at, last_active_at, status, title)
messages(id PK, session_id FK, role, content_json, token_count, created_at, idx)
events(id PK, session_id FK, type, payload_json, created_at, idx)
tool_calls(id PK, session_id FK, message_id FK, tool, input_json, output_json, ok, took_ms)
permissions(id PK, scope, tool, pattern, decision, created_at)
```

- `events` 是 append-only 事件流：所有 Server→Client 事件都落盘
- `messages` 是消息表：每条 LLM 完成态写一行（便于压缩/分页/检索）
- `tool_calls` 独立表便于审计、回放、构造测试 fixture
- 默认 DB 路径：`~/.dataagent/state.db`
- 可在 session 创建时设 `ephemeral=true` 跳过持久化

### 7.5 断线重连

```
1. 客户端断线
2. 服务端 session goroutine 继续跑（不依赖 ws 连接存活）
3. Server→Client 事件继续写入 events 表
4. 客户端重连：GET /v1/sessions/{id}/stream?since_event_idx=NNN
5. 服务端先发 NNN 之后的所有 events，再继续转发实时事件
```

LLM 推理不会因前端关闭/刷新而中断。

### 7.6 会话隔离

| 维度 | 隔离方式 |
|---|---|
| 工作目录 | 每 session 一个 `workdir`，工具调用的相对路径 resolve 到该目录 |
| 环境变量 | 每 session 独立 `Env`，Bash 工具用它 + 当前进程白名单变量 |
| 文件访问 | 默认拒绝 workdir 外路径，可在 session 配置加入额外允许路径 |
| LLM 历史 | 完全独立，通过 session_id 隔离 |
| 取消传播 | 父 session interrupt → ctx.Cancel() → 级联到子 session、子工具、MCP 调用 |

**这不是安全沙箱**——防误操作，不防恶意 prompt。README 明示。

---

## 8. 工具系统 + 并行执行

### 8.1 Tool 接口

```go
type Tool interface {
    Name() string
    Description() string
    InputSchema() jsonschema.Schema

    IsReadOnly(input json.RawMessage) bool
    CanRunInParallel(input json.RawMessage) bool

    PermissionPreview(input json.RawMessage) string

    Run(ctx context.Context, input json.RawMessage, env *ToolEnv) (*ToolResult, error)
}

type ToolEnv struct {
    Workdir       string
    SessionEnv    map[string]string
    Permissions   PermissionStore
    EmitEvent     func(Event)
    SessionStore  store.SessionStore
}

type ToolResult struct {
    Output           string
    IsError          bool
    ContextModifier  func(*ToolEnv)
    Took             time.Duration
}
```

### 8.2 并行执行编排算法

```
LLM 一轮返回多个 tool_use:
  [Read(a.go), Read(b.go), Bash("ls"), Edit(c.go), Bash("rm")]

Step 1: batchTools()  按 CanRunInParallel 分批（保留 LLM 给出的顺序）
  Batch 1 (parallel): Read(a.go), Read(b.go)
  Batch 2 (serial):   Bash("ls")
  Batch 3 (serial):   Edit(c.go)
  Batch 4 (serial):   Bash("rm")

Step 2: 逐批执行
  - 可并发批：errgroup + semaphore(MaxParallelTools=10) 并发跑，等全部完成
  - 串行批：顺序 await
  - 并发期间收集 contextModifier，批后顺序 apply

Step 3: 所有 tool_result 一起塞回 LLM history，进入下一轮
```

Go 实现要点：

```go
func (a *Agent) runToolBatch(ctx context.Context, batch ToolBatch, emit EventEmitter) []ToolResult {
    if !batch.Parallel || len(batch.Calls) == 1 {
        return a.runSerial(ctx, batch.Calls, emit)
    }
    sem := make(chan struct{}, a.cfg.MaxParallelTools)
    g, gctx := errgroup.WithContext(ctx)
    results := make([]ToolResult, len(batch.Calls))
    modifiers := make([][]ContextModifier, len(batch.Calls))
    for i, call := range batch.Calls {
        i, call := i, call
        g.Go(func() error {
            sem <- struct{}{}
            defer func() { <-sem }()
            if !a.perm.Allow(call) {
                results[i] = errResult("denied")
                return nil
            }
            r, err := call.Tool.Run(gctx, call.Input, a.env)
            results[i] = toResult(r, err)
            if r != nil && r.ContextModifier != nil {
                modifiers[i] = []ContextModifier{r.ContextModifier}
            }
            return nil
        })
    }
    g.Wait()
    for _, ms := range modifiers {
        for _, m := range ms { m(a.env) }
    }
    return results
}
```

### 8.3 设计决策

1. 单工具失败 ≠ 整批取消：并发批里某个工具报错只影响它自己的 tool_result，其他继续
2. 权限询问在并发批里也是独立的：3 个 Read 并发跑可能 3 个同时发出 `permission_request` 事件
3. 取消语义级联：`/cancel` → 取消 session ctx → 所有正在跑的 goroutine 收到 `ctx.Done()`
4. 并发上限可配置：`config.MaxParallelTools`（默认 10）

### 8.4 内置 5 工具的并发标志

| 工具 | IsReadOnly | CanRunInParallel | 备注 |
|---|---|---|---|
| `Read` | true | true | 始终安全 |
| `Grep` | true | true | 始终安全 |
| `Bash` | false（MVP 简化） | false | MVP 永远串行；V2 加智能分类 |
| `Edit` | false | false | 永远串行 |
| `Write` | false | false | 永远串行 |

### 8.5 Bash 安全：仅"重大隐患"防护

MVP 不做命令分类，但维护一个 DangerousCommandGuard：

```go
var dangerousPatterns = []*regexp.Regexp{
    regexp.MustCompile(`\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r)\s+/\S*`),
    regexp.MustCompile(`\brm\s+-[a-zA-Z]*r[a-zA-Z]*f\s+~`),
    regexp.MustCompile(`\bdd\s+.*\bof=/dev/`),
    regexp.MustCompile(`\bmkfs\.\w+\s+/dev/`),
    regexp.MustCompile(`>\s*/dev/(sd|nvme|hd)`),
    regexp.MustCompile(`:\(\)\s*{\s*:\s*\|\s*:\s*&\s*}`),
    regexp.MustCompile(`\bchmod\s+-R\s+777\s+/`),
    regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`),
}
```

行为：
- 正常命令：走常规权限链
- 命中危险模式：**强制弹询问，绕过任何 `allow_permanent` 规则**，事件标记 `risk: "catastrophic"`，UI 应红色高亮 + 二次确认

### 8.6 权限模型

```go
type Permission struct {
    Tool    string    // "Bash" / "Edit"
    Pattern string    // 工具特定的匹配规则
    Decision string   // "allow" | "deny" | "ask"
    Scope   string    // "session" | "permanent" | "once"
}
```

询问决策选项：
- `allow_once`：仅本次
- `allow_session`：本 session 同模式
- `allow_permanent`：写入 SQLite 跨 session 永久
- `deny`：仅本次
- `deny_permanent`：永久拒绝

危险命令强制询问，绕过 permanent 规则。

---

## 9. 多 agent 协作（"小龙虾"模式）

### 9.1 模型

主 agent 是普通 session，子 agent 也是普通 session。差别只在"谁触发它、结果回给谁"。

```
父 session #A
   │ LLM 决定调 Dispatch 工具：Dispatch(prompt="...", agent_type="researcher", ...)
   ▼
[Dispatch 工具内部]
   ├─ store.CreateSession(parent=A, type="sub")
   ├─ 把 prompt 作为子 session 初始 user_message
   ├─ 子 session 在自己的 goroutine 跑（独立 inbox/outbox + history）
   ├─ 父等待模式：
   │    • blocking：父 goroutine select 等子完成事件
   │    • streaming：父立刻拿到 sub_id，订阅子事件流（默认）
   └─ 子完成 → final assistant 文本作为 tool_result 返父
```

### 9.2 Dispatch 工具

```go
type DispatchInput struct {
    Prompt       string            `json:"prompt"`
    AgentType    string            `json:"agent_type,omitempty"`
    Inputs       map[string]any    `json:"inputs,omitempty"`
    Mode         string            `json:"mode"`              // "blocking" | "streaming"，默认 "streaming"
    Workdir      string            `json:"workdir,omitempty"`
    AllowedTools []string          `json:"allowed_tools,omitempty"`
}

func (t *DispatchTool) CanRunInParallel(input json.RawMessage) bool { return true }
```

`CanRunInParallel=true` → 父 agent 一轮里可以并发 dispatch 多个子 agent。

### 9.3 子 agent 隔离规则

| 维度 | 隔离策略 |
|---|---|
| History | 完全独立 |
| Workdir | 默认继承，可覆盖 |
| Env | 默认继承 |
| Tools | 默认所有，可用 AllowedTools 限制 |
| MCP | 共享父的 MCP host |
| Provider/Model | 子 agent 可在 `agent_type.yaml` 或 Dispatch 参数指定；缺省继承父 |
| Token 计量 | 各自统计；父 session 计入 + 子 session 累加 |
| 持久化 | 子 session 有 `parent_session_id` 外键，可独立查询/回放 |
| 取消 | 父 ctx 取消 → 级联取消所有子 |
| 错误 | 子 agent 抛错 → 转 tool_result 的 `is_error=true` 返父 |

### 9.4 Agent 角色配置

```yaml
# ~/.dataagent/agents/researcher.yaml
name: researcher
description: 独立调研任务，产出结构化总结
system_prompt: |
  你是一个研究型子 agent。专注于信息收集和综合。
  完成后输出 markdown 总结。
tools:
  allow: [Read, Grep, WebFetch, WebSearch]
  deny: [Edit, Write, Bash]
provider: anthropic              # 可选
model: claude-sonnet-4-6         # 可选
max_iterations: 20
```

- Dispatch 的 `agent_type` 参数 → 找到对应配置 → system_prompt 注入子 session
- 角色文件**热加载**，session 启动时扫描

### 9.5 事件透传

- `streaming` 模式（默认）：子 agent 的 token 流、工具调用都以 `subagent_event` 事件透传给父客户端
- `blocking` 模式：父客户端只看到 dispatch 的开始与结束，过程不透出

---

## 10. Provider 抽象

### 10.1 Provider 接口

```go
type Provider interface {
    Name() string
    Stream(ctx context.Context, req Request) (<-chan ProviderEvent, error)
}

type Request struct {
    Model       string
    System      string
    Messages    []Message
    Tools       []ToolSchema
    MaxTokens   int
    Temperature float64
    Stream      bool
}

type ProviderEvent struct {
    Type       string    // "text_delta" | "tool_use" | "stop" | "usage" | "error"
    TextDelta  string
    ToolUse    *ToolUseBlock
    Stop       *StopReason
    Usage      *Usage
    Err        error
}
```

### 10.2 内部统一 Message 格式

以 Anthropic Messages 语义为模板（tool use 表达力最强）：

```go
type Message struct {
    Role     string         // "user" | "assistant"
    Blocks   []ContentBlock
}

type ContentBlock struct {
    Type      string          // "text" | "tool_use" | "tool_result"
    Text      string
    ToolUseID string
    ToolName  string
    Input     json.RawMessage
    Output    string
    IsError   bool
}
```

### 10.3 Adapter

- **AnthropicAdapter**：内部 Message ↔ Anthropic Messages SSE（几乎 1:1，最轻）
- **OpenAIAdapter**：
  - 入站：`tool_use` block → `assistant.tool_calls[]`；`tool_result` block → 独立 `role:"tool"` 消息
  - 出站：OpenAI SSE `delta.tool_calls` 流式累积 → 在 `finish_reason=tool_calls` 时 emit `tool_use` 事件
  - 已知差异：OpenAI 单轮工具数量上限（取决于 model）；文本与 tool_use 不能交错

### 10.4 兼容范围声明

- 官方测试通过：Anthropic Messages API、OpenAI Chat Completions API
- OpenAI 兼容的国内模型（DeepSeek / Qwen / 豆包 / Ollama）：可通过 `base_url` 接入，**不保证、不维护、文档明示**

### 10.5 模型选择策略

```go
type ModelPolicy struct {
    Default       string
    Fast          string                 // compaction / 小任务用
    BySessionType map[string]string
}
```

session 创建时可指定；agent 角色配置可指定；都缺省则用 Default。
压缩用 Fast 模型，**同家原则**：Anthropic session → Haiku；OpenAI session → `gpt-4o-mini`。

### 10.6 自实现 HTTP 客户端

不引入 `anthropic-sdk-go` / `openai-go`，自己写薄客户端。理由：

- SDK 演进太快，依赖锁定不灵活
- 自实现易控、合规留痕（每行代码都是我们的）
- 仅需 SSE 解析 + HTTP POST，工程量小

---

## 11. 错误处理 + 取消 + 上下文压缩

### 11.1 错误分类

| 类别 | 例子 | 处理 | 入 history |
|---|---|---|---|
| Tool 业务错误 | Read 文件不存在；Bash exit≠0 | 包装 `tool_result.is_error=true`，文本写明 | ✅ |
| Tool 系统错误 | 工具崩溃、超时 | recover → `tool_result.is_error=true`，标记 `internal_error` | ✅，记 events 表 |
| Provider 可重试 | 429、503、网络抖动 | 指数退避 3 次（500ms / 2s / 8s），emit `provider_retry` | ❌ |
| Provider 不可重试 | 401、400、context_length_exceeded | 立即停止，emit `error`，回 Idle | ❌ |
| Session 级 panic | 未捕获异常 | 顶层 recover → events 表 → 状态 `crashed` | ✅ |

### 11.2 取消（Cancel / Interrupt）

```
POST /v1/sessions/{id}/cancel  或  ws message {type:"interrupt"}
        │
        ▼
session.cancel() → ctx.CancelFunc 级联：
   ├─ provider.Stream：HTTP req ctx 取消，LLM 流被砍
   ├─ 正在跑的工具：
   │    • Bash: exec.CommandContext + 进程组 SIGTERM（1.5s 后 SIGKILL）
   │    • Read/Grep: 早返回
   │    • WebFetch: HTTP req ctx 取消
   │    • MCP tool: 转发 cancel 到 MCP server
   │    • Dispatch: 级联取消子 session
   ▼
session 收尾：
  • 半完成的 LLM 响应不入 history
  • 已完成的 tool_result 保留
  • 未完成的 tool_result：写入合成 {is_error: true, output: "<cancelled by user>"}，让下一轮 LLM 知道
  • emit `interrupted` 事件
  • 状态回到 Idle
```

取消是幂等的；连续多次取消无副作用。

### 11.3 上下文压缩

**触发条件**（任一）：
- token 使用量 > 模型上下文窗口 × 0.85
- 显式调 `/v1/sessions/{id}/compact`

**流程**：
1. session 进入 `Compacting` 状态（拒绝新消息，但不取消当前推理）
2. 把前 N-K 条用 Fast 模型总结：
   - 保留最近 K 条原始消息（K=8 默认）
   - 保留所有 `is_error=true` 的 tool_result
   - 输出单条 system-like 消息 `<summary>...</summary>`
3. 新 history = `[summary]` + `[recent K messages]`
4. emit `compaction { before: 12000, after: 3500 }`
5. 回到 Idle

注意：压缩有损。messages 表保留所有原始消息（压缩只改"喂给 LLM 的 history"）。UI 可随时看完整对话。

---

## 12. 测试策略

### 12.1 金字塔

```
            ┌──────────────┐
            │  E2E (少量)   │  真二进制 + 真 SQLite + mock LLM server
            └──────────────┘
          ┌──────────────────┐
          │  Integration     │  session + agent + provider mock
          │  (中等)            │
          └──────────────────┘
      ┌────────────────────────┐
      │  Unit (大量)             │  纯函数
      └────────────────────────┘
```

### 12.2 各层覆盖

| 层 | 工具 | 覆盖 |
|---|---|---|
| Unit | std `testing` + table-driven | batch 划分、danger 分类、permission 匹配、message 格式转换、compaction 触发 |
| Integration | 内存 SQLite + provider mock（fake Provider 输出预设事件流） | session 状态机、tool orchestration、子 agent dispatch、取消传播 |
| E2E | 启动真二进制 + `httptest.Server` 假 LLM | WebSocket 握手、事件序列、断线重连、并发会话隔离 |

### 12.3 关键约定

- Provider mock 用 **SSE 录制回放**：录一次真实响应到 JSON fixture，测试时回放，避免 token 消耗与稳定性问题
- 所有 `go test` 默认 `-race -timeout 30s`

---

## 13. 可观测性（MVP 范围）

| 类型 | 实现 |
|---|---|
| 结构化日志 | `log/slog` JSON 格式 |
| 健康检查 | `GET /health` |
| Debug 端点 | `GET /debug/sessions/{id}/events?since=N` |

V2 才加：metrics（expvar / Prometheus）、tracing（OpenTelemetry）。

日志要求：

```go
logger.Info("tool.completed",
    "session_id", sid,
    "tool", "Bash",
    "input_hash", h,         // 不打 input 原文，打 hash
    "ok", true,
    "took_ms", 42,
    "request_id", reqID,
)
```

默认 INFO 级别；`log_llm_payload: false` 默认关。

---

## 14. 目录结构 + 主要依赖

### 14.1 目录

```
data-agent/
├── cmd/dataagent/main.go
├── internal/
│   ├── api/
│   │   ├── server.go
│   │   ├── handler_sessions.go
│   │   ├── handler_stream.go
│   │   ├── handler_health.go
│   │   ├── middleware.go
│   │   └── protocol/
│   │       ├── client_events.go
│   │       └── server_events.go
│   ├── session/
│   │   ├── manager.go
│   │   ├── session.go
│   │   ├── state.go
│   │   └── inbox.go
│   ├── agent/
│   │   ├── loop.go
│   │   ├── orchestrator.go
│   │   ├── compaction.go
│   │   └── modelpolicy.go
│   ├── provider/
│   │   ├── provider.go
│   │   ├── message.go
│   │   ├── anthropic/
│   │   └── openai/
│   ├── tools/
│   │   ├── tool.go
│   │   ├── registry.go
│   │   ├── readfile/
│   │   ├── writefile/
│   │   ├── editfile/
│   │   ├── grep/
│   │   ├── bash/
│   │   │   ├── tool.go
│   │   │   ├── danger.go
│   │   │   └── exec.go
│   │   └── dispatch/
│   ├── permission/
│   ├── coord/
│   ├── mcp/
│   ├── skills/
│   ├── store/sqlite/
│   └── config/
├── pkg/client/                  # 给嵌入用户的 Go SDK
├── web/                         # 嵌入式参考 UI
├── docs/
├── test/
│   ├── e2e/
│   ├── fixtures/                # provider SSE 录制
│   └── mockprovider/
├── scripts/
├── go.mod / go.sum
├── LICENSE / README.md / CLAUDE.md
```

### 14.2 主要依赖

| 用途 | 依赖 | 理由 |
|---|---|---|
| HTTP router | `github.com/go-chi/chi/v5` | 轻量、惯用 |
| WebSocket | `nhooyr.io/websocket` | 现代实现 |
| SQLite | `modernc.org/sqlite` | 纯 Go、无 CGO |
| 配置 | `gopkg.in/yaml.v3` | yaml 解析 |
| JSON Schema | `github.com/santhosh-tekuri/jsonschema/v5` | 工具入参验证 |
| 并发 | `golang.org/x/sync/errgroup` + `semaphore` | 并发批 |
| Logging | `log/slog` (std) | 不引外部 |
| ULID | `github.com/oklog/ulid/v2` | 时间有序 ID |
| 命令解析 | `github.com/google/shlex` | Bash 分词 |

依赖克制原则：除上述外，引入新依赖需写理由。

---

## 15. 构建与分发

- `go build -trimpath -ldflags="-s -w"` → 12-18MB 静态二进制
- 多平台 matrix：Linux / macOS / Windows × amd64 / arm64
- 嵌入参考 UI：`//go:embed web/*` → `http://127.0.0.1:7821/ui`
- 首次启动自动创建 `~/.dataagent/{config.yaml, state.db, mcp.json, skills/, agents/}`
- CLI 命令：
  - `dataagent init`：交互式生成配置
  - `dataagent serve`：启动服务
  - `dataagent version`：版本信息
- CI：GitHub Actions → `go vet` + `staticcheck` + `go test -race` + multi-arch build

---

## 16. 非目标（明确不进 MVP）

- Hooks 机制
- ACP 协议兼容（事件结构留扩展空间，但不实现 ACP 规范）
- 多租户 / 完整鉴权系统（仅单 token）
- 容器 / bubblewrap 沙箱
- Web UI 美化
- 自动更新
- 插件市场 / skills 仓库
- Vim / Emacs 模式
- Prometheus 指标
- Voice / 多模态
- 远程会话 / 集群
- 提示词管理界面

每项都进 V2+ 的独立子项目 spec。

---

## 17. 术语表

| 术语 | 含义 |
|---|---|
| Session | 一次多轮对话上下文，含 history、workdir、env |
| Agent loop | LLM 调用 + 工具批 + 回灌 + 再调 LLM 的循环 |
| Tool | 单次可调用的能力（Read/Bash/Dispatch 等） |
| Provider | LLM 后端（anthropic / openai） |
| Dispatch | 父 agent 启动子 agent 的工具 |
| Sub-agent | 由父 session Dispatch 出来的子 session |
| MCP | Anthropic 公开的 Model Context Protocol，工具扩展协议 |
| Skill | 按需加载的能力包，含 trigger + content |
| Compaction | 上下文压缩，把老消息总结成单条 summary |
| Permission | 工具调用前的允许 / 询问 / 拒绝决策 |
| Event | WebSocket 上传输的单条消息；append-only 存 events 表 |
| ULID | 时间有序的 128-bit 唯一标识符 |

---

## 18. 实施计划入口

设计文档复审通过后，由 brainstorming 流程交接给 `writing-plans` 技能产出实施计划（步骤拆分、依赖关系、里程碑）。
