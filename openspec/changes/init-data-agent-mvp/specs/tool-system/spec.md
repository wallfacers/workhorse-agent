## ADDED Requirements

### Requirement: Tool 接口契约

每个工具 SHALL 实现以下 Go 接口：

```go
type Tool interface {
    Name() string
    Description() string
    InputSchema() jsonschema.Schema
    IsReadOnly(input json.RawMessage) bool
    CanRunInParallel(input json.RawMessage) bool
    PermissionPreview(input json.RawMessage) string
    DefaultTimeout() time.Duration
    Run(ctx context.Context, input json.RawMessage, env *ToolEnv) (*ToolResult, error)
}
```

`Run` 返回的 `ToolResult` 含 `Output`（LLM 可见文本）、`IsError`、可选 `ContextModifier`（声明对 `ToolEnv` 的副作用）、`Took`（耗时）。

`Run` SHALL 尊重 `ctx`；超时或被取消时 SHALL 在 100ms 内返回。

#### Scenario: 工具响应 ctx 取消

- **WHEN** 工具正在执行时其 `ctx` 被 cancel
- **THEN** 工具在 100ms 内返回；返回的 error 可以为 `context.Canceled`

<!-- 来源：AI #2 复审 (2026-05-24) M-7：除 Bash 外其他工具无全局超时保护，大文件/网络文件系统/慢 MCP 工具可能阻塞数分钟。 -->

### Requirement: 工具执行全局超时

Agent orchestrator 在调用 `tool.Run(ctx, ...)` 前 SHALL 用 `context.WithTimeout` 套一层超时上限，按以下优先级取超时值：

1. 工具自身声明的 `DefaultTimeout()`（每个内置工具实现自报）
2. 配置 `tools.<tool_name>.timeout_seconds`（用户级覆盖）
3. 全局默认 `tools.default_timeout_seconds`（默认 60 秒）

内置 5 工具的默认超时：

| 工具 | DefaultTimeout | 备注 |
|---|---|---|
| `Read` | 30s | 大文件读用 offset/limit 控制 |
| `Grep` | 60s | 大目录搜索可能慢 |
| `Write` | 30s | 通常很快 |
| `Edit` | 30s | 通常很快 |
| `Bash` | 取 input.timeout 参数（默认 `tools.bash.timeout_seconds` = 120s） | Bash 自管，不被这条 wrapping 二次包；orchestrator 仍 wrap 以保护 Bash 实现 bug 卡死 |

MCP 工具与 LoadSkill 等动态工具 SHALL 使用全局 `tools.default_timeout_seconds`，除非 MCP server 在 tool metadata 中声明 `timeout_seconds`。

超时触发时 SHALL 包装为 `tool_result { is_error: true, output: "tool execution timed out after Ns" }`，与正常工具失败同样回灌 LLM。

#### Scenario: Read 超时

- **WHEN** Read 工具在网络文件系统上读取慢文件，60s 仍未完成
- **THEN** orchestrator 触发 ctx 超时；Read 100ms 内返回；tool_result 含 `{ is_error: true, output: "tool execution timed out after 30s" }`

#### Scenario: 配置覆盖默认超时

- **WHEN** `tools.grep.timeout_seconds: 300`，Grep 工具被调用
- **THEN** orchestrator 用 300s 超时（覆盖默认 60s）

#### Scenario: MCP 工具 metadata 声明优先

- **WHEN** MCP server 在 tools/list 中声明某工具 `timeout_seconds: 600`
- **THEN** orchestrator 用 600s 超时调该 MCP 工具

### Requirement: 内置 5 工具

服务 SHALL 内置注册以下 5 个工具：

| 工具 | IsReadOnly | CanRunInParallel | 行为 |
|---|---|---|---|
| `Read` | true | true | 读 `path` 文件内容（支持 offset/limit/pages） |
| `Grep` | true | true | 在 `path` 下搜索 `pattern`（regex），返回匹配行 |
| `Write` | false | false | 写 `content` 到 `path`（覆盖） |
| `Edit` | false | false | 在 `path` 中把 `old_string` 替换为 `new_string`（exact-match） |
| `Bash` | false（MVP 简化） | false | 在会话 workdir 中执行 `command`，最长 `timeout` 秒，返回 stdout+stderr |

所有工具 SHALL 强制路径在会话 workdir 内（或额外 allowed_paths 内），否则返回 `is_error: true`。**路径校验的具体算法**（filepath.Clean → EvalSymlinks → filepath.Rel → O_NOFOLLOW / Lstat 复检）由 `session-management` capability 的 "路径越界防护算法" requirement 定义；本 capability 的所有工具实现 SHALL 通过统一的 `internal/tools/pathguard` 模块调用该算法，不得自行实现。MCP 工具适配层、Skills 的 LoadSkill 注入工具、未来动态工具同样 SHALL 经过 pathguard。

#### Scenario: Edit 未找到 old_string

- **WHEN** 调用 `Edit { path, old_string: "foo", new_string: "bar" }`，文件中无 "foo"
- **THEN** 返回 `tool_result { is_error: true, output: "old_string not found in file" }`，不修改文件

#### Scenario: Bash 在会话 workdir 中执行

- **WHEN** 会话 workdir 是 `/tmp/p`，调用 `Bash { command: "pwd" }`
- **THEN** 工具返回的 output 含 `/tmp/p`

### Requirement: 并行执行批次划分

Agent SHALL 对 LLM 一轮返回的 `tool_use[]` 按以下规则切批：

1. 保留 LLM 给出的顺序
2. 连续多个 `CanRunInParallel(input)=true` 的工具调用合并为一个 parallel batch
3. 任何 `CanRunInParallel(input)=false` 的工具调用单独成一个 serial batch
4. 批与批之间 SHALL 严格顺序执行（不重排序）
5. parallel batch 内 SHALL 并发执行，受 `MaxParallelTools`（默认 10）信号量限流

#### Scenario: 混合工具切批

- **WHEN** LLM 返回 `[Read(a), Read(b), Edit(c), Read(d), Bash("ls")]`
- **THEN** 切成 4 批：`[Read(a), Read(b)]` 并发 → `[Edit(c)]` 串行 → `[Read(d)]` 串行 → `[Bash("ls")]` 串行（因 Bash MVP CanRunInParallel=false）

#### Scenario: 全并发批

- **WHEN** LLM 返回 `[Read(a), Read(b), Read(c), Grep(d)]`
- **THEN** 切成 1 批，4 个工具并发执行

### Requirement: 并发批内单工具失败不取消整批

并发批内某个工具 `Run` 返回 error 或 panic SHALL **不**触发批的 ctx 取消；其他工具继续；该工具的结果包装为 `tool_result { is_error: true, output: error.Error() }`。

唯有 session 级 ctx 取消（用户 interrupt）SHALL 取消整批。

#### Scenario: 并发批一工具失败

- **WHEN** 并发批 `[Read(a), Read(b)]` 中 `Read(a)` 因文件不存在返回 error
- **THEN** `Read(b)` 继续执行至完成；批整体完成；history 追加 2 个 tool_result，其中 a 的 `is_error=true`

### Requirement: ContextModifier 延迟应用

并发批内多个工具的 `ToolResult.ContextModifier` SHALL 在整批完成后按工具的原始顺序顺序 apply 到 `ToolEnv`，避免并发写竞争。

子 agent 通过 `Dispatch` 工具返回的 `ToolResult` SHALL **不**含 `ContextModifier`（子 session 独立 ToolEnv，理论上不应修改父）；若子 session 内部产生了 ContextModifier，它们仅在子 ToolEnv 内 apply，不传播到父。

#### Scenario: 并发工具的 modifier 顺序应用

- **WHEN** 并发批中工具 1 和工具 2 都返回 ContextModifier，两 modifier 都修改 `env.SessionEnv["KEY"]`
- **THEN** 批完成后先 apply 工具 1 的 modifier，再 apply 工具 2 的 modifier；最终 `env.SessionEnv["KEY"]` 取工具 2 的修改值

#### Scenario: 子 agent 的 ContextModifier 不传播到父

- **WHEN** 父 agent 并发 dispatch 2 个子 agent，子 agent 内部工具产生 ContextModifier 修改了子的 SessionEnv
- **THEN** 父的 ToolEnv 不受影响；Dispatch 返回的 tool_result 的 ContextModifier 字段为 nil

### Requirement: 工具注册表与动态发现

服务 SHALL 维护全局 ToolRegistry，启动时注册 5 个内置工具；MCP 工具与 Skills 触发的 LoadSkill 注入工具 SHALL 通过同一接口注册。

ToolRegistry 的注册、查询、删除操作 SHALL 用 `sync.RWMutex` 保护，确保运行时多 goroutine（MCP server 启动/重启、LoadSkill 动态注入、session lookup）并发访问安全。Lookup 是高频读操作 SHALL 用 RLock。

会话级 SHALL 支持工具子集：通过 `AllowedTools` 配置可限制本会话可调工具范围。

#### Scenario: 子集限制

- **WHEN** 会话配置 `AllowedTools: ["Read", "Grep"]`，LLM 请求调用 `Bash`
- **THEN** Agent 不向 LLM 暴露 Bash schema；若 LLM 仍尝试调用，emit `error { code: "tool_not_allowed" }`

<!-- 来源：AI #2 复审 (2026-05-24) M-6：Bash env 过滤规则仅在 tasks 5.7 中，spec 层无 SHALL 保护，安全相关规则应进 spec。 -->

### Requirement: Bash 环境变量隔离

Bash 工具在执行 `exec.CommandContext` 前 SHALL 对继承的进程 env 按下表规则过滤（实现 SHALL 在 `internal/tools/bash/envfilter.go` 一处集中维护，便于审计）：

| 变量名（精确匹配，区分大小写） | 判定 | 备注 |
|---|---|---|
| `LD_PRELOAD` | **移除整条** | 动态链接器注入 |
| `LD_LIBRARY_PATH` | **移除整条** | 库路径劫持 |
| `LD_AUDIT` | **移除整条** | 审计回调注入 |
| `DYLD_INSERT_LIBRARIES` | **移除整条** | macOS 等价 LD_PRELOAD |
| `DYLD_LIBRARY_PATH` | **移除整条** | macOS 等价 LD_LIBRARY_PATH |
| `DYLD_FALLBACK_LIBRARY_PATH` | **移除整条** | macOS 库路径回退 |
| `DYLD_FORCE_FLAT_NAMESPACE` | **移除整条** | macOS 符号空间劫持 |
| `DYLD_*`（其余以 `DYLD_` 前缀开头者） | **移除整条** | 防新增 DYLD_ 攻击面 |
| `PYTHONPATH` | **移除整条** | MVP 简化策略：不做选择性过滤；用户需自定义可通过 session env 显式声明，但仍受下方"合并不能再引入"约束 |
| `PYTHONSTARTUP` | **移除整条** | Python 启动钩子 |
| `NODE_OPTIONS` | **条件移除**：若值（经 shell 词法拆分后）含任一 token 以 `--require`、`--import`、`--experimental-loader`、`--inspect`、`--inspect-brk` 开头，**整条移除**；否则保留 | 这些 flag 可加载任意模块 |
| 其他 | **保留** | 含 `PATH`、`HOME`、`USER`、`SHELL`、`LANG`、`LC_*`、`TERM`、`TZ`、provider API key（仅子 session 继承）等 |

NODE_OPTIONS 判定算法 SHALL 用 `github.com/google/shlex` 或等价 POSIX shell-quoting 词法器拆分值后逐 token 检查前缀（避免简单子串匹配把合法值如 `NODE_OPTIONS=--no-deprecation` 误杀）。

会话级 `env` 字段（创建 session 时传入）SHALL 在过滤后的 base env 之上**合并**，但合并器 SHALL 对每个用户提供的 key 重新跑同一过滤规则：黑名单 key 被丢弃，启动日志 `warn` 级别记录 `dropped_session_env_key=<key> reason=blacklisted`，不暴露 value。

过滤目的是降低恶意命令通过 env 劫持动态链接器、Python/Node import 路径的风险。该过滤 SHALL **不**应用于 LLM provider HTTP 调用进程（仅工具子进程）。

#### Scenario: LD_PRELOAD 被剥离

- **WHEN** dataagent 启动进程的 env 含 `LD_PRELOAD=/tmp/evil.so`，Bash 工具被调用执行 `printenv LD_PRELOAD`
- **THEN** Bash 子进程的 env 不含 `LD_PRELOAD`；命令输出为空行

#### Scenario: 会话 env 不能注入黑名单

- **WHEN** 客户端创建 session 时传 `env: { "LD_PRELOAD": "/tmp/x.so" }`
- **THEN** session 创建成功但启动日志 `warn` 标记 `LD_PRELOAD` 已被丢弃；Bash 工具运行时 env 不含该变量

<!-- 来源：AI #2 复审 (2026-05-24)：tool_result.output 类型与大小缺契约，实现者可能写出非字符串或超大 payload 让 LLM 解析失败导致循环。 -->

### Requirement: ToolResult 输出格式与大小限制

`ToolResult.Output` SHALL 是 UTF-8 字符串（不是任意 JSON / bytes），目的是与 Anthropic Messages 与 OpenAI Chat Completions 的 `tool_result.content` / `role:"tool".content` 字段直接对应。

`Output` 的字节长度 SHALL **不超过** `tools.tool_result_max_bytes`（默认 1 MiB = 1048576）：

- 工具实现 SHALL 在返回前自我截断，超长时尾部追加单行截断标记 `\n\n[truncated: original size N bytes, kept first M bytes]`
- Bash / Grep / Read 等可能产生大输出的工具 SHALL 显式实现截断（如 Read 的 `limit` 参数、Bash 的输出 ring buffer）
- 截断后的 output 仍 SHALL 是合法 UTF-8（不切断多字节字符；若末尾出现部分字符，丢弃该部分）
- 截断的 ToolResult `IsError` SHALL 仍按工具语义判定（不因截断本身置 true）

对必须返回结构化数据的工具（如 MCP 工具的结构化 result），适配层 SHALL 在适配阶段把结构序列化为人类可读 JSON 字符串再赋给 `Output`，并在 LLM 系统提示中说明"tool_result 内容可能是 JSON 文本，需自行解析"。

#### Scenario: 大输出被截断

- **WHEN** Bash 执行 `yes | head -c 5000000`（5 MiB 输出），`tool_result_max_bytes` 默认 1 MiB
- **THEN** ToolResult.Output 首部为前约 1 MiB 字节、UTF-8 安全；尾部含 `\n\n[truncated: original size 5000000 bytes, kept first 1048576 bytes]`；`IsError=false`

#### Scenario: UTF-8 多字节边界

- **WHEN** Read 读取一个 UTF-8 文件，截断点恰好落在某个 3 字节中文字符中间
- **THEN** 截断回退到该字符之前；最终 Output 仍是合法 UTF-8

#### Scenario: 结构化 MCP 工具结果

- **WHEN** MCP 工具返回 `{ "items": [...], "next_cursor": "..." }` 结构化 JSON
- **THEN** MCP adapter 用 `json.MarshalIndent(..., "", "  ")` 序列化为字符串再赋给 ToolResult.Output；超 1 MiB 时按上述规则截断
