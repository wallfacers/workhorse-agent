## ADDED Requirements

### Requirement: Dispatch 工具

服务 SHALL 内置 `Dispatch` 工具，签名：

```go
type DispatchInput struct {
    Prompt       string         `json:"prompt"`           // 必需
    AgentType    string         `json:"agent_type,omitempty"`
    Inputs       map[string]any `json:"inputs,omitempty"`
    Mode         string         `json:"mode,omitempty"`   // "blocking" | "streaming"，默认 "streaming"
    Workdir      string         `json:"workdir,omitempty"`
    AllowedTools []string       `json:"allowed_tools,omitempty"`
    Provider     string         `json:"provider,omitempty"`
    Model        string         `json:"model,omitempty"`
}
```

`Dispatch.CanRunInParallel(input)` SHALL 返回 `true`，使父 agent 可一轮并发派发多个子 session。

#### Scenario: 父 agent 一轮并发派发

- **WHEN** 父 agent LLM 返回 `[Dispatch(researcher, X), Dispatch(researcher, Y), Dispatch(coder, Z)]`
- **THEN** 3 个子 session 在同一个 parallel batch 内并发启动，各自独立运行 agent 循环

### Requirement: 子 session 隔离

子 session SHALL：

- 有独立的 `history`（不见父 history）
- 默认继承父 session 的 `workdir`、`env`、`provider`、`model`（Dispatch 参数或 agent_type 配置可覆盖）
- 默认拥有所有工具（可通过 `AllowedTools` 限制）
- 共享父 session 的 MCP host（避免每子重启 MCP server）
- 在 SQLite `sessions` 表中以 `parent_id` 外键关联父
- 独立的 token 计量

#### Scenario: 子继承父的 workdir 但 history 独立

- **WHEN** 父 workdir `/proj`，已对话 5 轮；Dispatch 启子且未覆盖 workdir
- **THEN** 子 session 的 `workdir` 是 `/proj`；子的 history 长度为 1（仅初始 user_message 是 Dispatch 的 prompt）

#### Scenario: 子覆盖 provider/model

- **WHEN** 父用 `anthropic`/`claude-sonnet-4-6`；Dispatch 参数 `{ provider: "openai", model: "gpt-4o" }`
- **THEN** 子 session 用 OpenAI provider 调 `gpt-4o`；父继续用 Anthropic

### Requirement: Agent 角色配置

服务 SHALL 在启动时及每次 Dispatch 调用前扫描 `~/.dataagent/agents/*.yaml`（热加载），加载 Agent 角色配置：

```yaml
name: <kebab-case>
description: <短描述>
system_prompt: |
  <多行 system prompt>
tools:
  allow: [Read, Grep, ...]   # 可选；缺省全部
  deny: [Bash, ...]           # 可选
provider: anthropic            # 可选
model: claude-sonnet-4-6       # 可选
max_iterations: 20             # 可选；默认 50
```

Dispatch 的 `agent_type` 参数 SHALL 用此 name 匹配；命中后 system_prompt 作为子 session 的系统消息。

#### Scenario: agent_type 不存在

- **WHEN** Dispatch `{ agent_type: "unknown-role" }`
- **THEN** Dispatch 返回 `tool_result { is_error: true, output: "agent_type not found: unknown-role" }`，不启动子 session

#### Scenario: 修改 yaml 后立即生效

- **WHEN** 用户编辑 `~/.dataagent/agents/researcher.yaml` 改 system_prompt 并保存
- **THEN** 下一次 Dispatch `agent_type: researcher` 用新的 system_prompt（无需重启服务）

### Requirement: 事件透传模式

`Dispatch.Mode` SHALL 控制子 session 事件透传到父客户端：

- `streaming`（默认）：子 session 的所有 Server→Client 事件 SHALL 被包装为 `subagent_event { agent_id, event: <inner> }` 透传给父 WebSocket
- `blocking`：父客户端 SHALL 仅看到 Dispatch 工具的 `tool_call_start` 和 `tool_call_done`，子 session 过程 SHALL 不透传

子 session 完成后 Dispatch 工具 SHALL emit `tool_call_done` 含子 session 的 final assistant 文本作为 output。

#### Scenario: streaming 模式透传

- **WHEN** 父 Dispatch 子 `mode: "streaming"`，子 session emit `assistant_text_delta`
- **THEN** 父 WebSocket 收到 `subagent_event { agent_id: <child_id>, event: { type: "assistant_text_delta", delta: "..." } }`

#### Scenario: blocking 模式不透传

- **WHEN** 父 Dispatch 子 `mode: "blocking"`
- **THEN** 父 WebSocket 仅收到 `tool_call_start` 和最终 `tool_call_done`，无中间 `subagent_event`

### Requirement: 取消级联到子 session

父 session 的 `context.CancelFunc` 被触发时 SHALL 级联取消所有正在运行的子 session（递归）。子 session 单独被取消 SHALL 不影响父。

#### Scenario: 父取消则子也取消

- **WHEN** 父 session 有 2 个跑中的子 session，父收到 `interrupt`
- **THEN** 父 ctx 取消 → 2 个子 ctx 取消 → 子内部的工具/LLM 调用全部停止；emit `subagent_event { event: { type: "interrupted" } }` × 2

### Requirement: 嵌套深度上限

服务 SHALL 维护 `max_depth`（默认 5）；session 的嵌套深度（父→子→孙...）达到上限后 SHALL 拒绝 `Dispatch` 调用。

#### Scenario: 超深度拒绝

- **WHEN** 当前 session 已是 depth=5 的子孙级，其 LLM 尝试再 Dispatch
- **THEN** Dispatch 返回 `tool_result { is_error: true, output: "max sub-agent depth (5) exceeded" }`

### Requirement: 子 session 错误隔离

子 session 抛错（含 panic）SHALL 被 Dispatch 工具捕获并转成父 agent 看到的 `tool_result { is_error: true, output: <error message> }`，**不**让父 session 崩溃。

#### Scenario: 子 panic 不影响父

- **WHEN** 子 session 内部 panic
- **THEN** 顶层 recover 将 panic 转为 error；Dispatch 工具返回 `tool_result { is_error: true }`；父 session 继续运行
