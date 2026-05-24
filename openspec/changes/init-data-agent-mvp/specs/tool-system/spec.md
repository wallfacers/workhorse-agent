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

所有工具 SHALL 强制路径在会话 workdir 内（或额外 allowed_paths 内），否则返回 `is_error: true`。

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
