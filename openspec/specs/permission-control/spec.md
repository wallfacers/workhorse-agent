# permission-control Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: 权限规则数据结构与匹配语义

服务 SHALL 维护权限规则集合，每条规则含：

```go
type Permission struct {
    Tool     string  // 工具名 glob 模式："Bash" / "dataweave__query_*" / "*"（匹配任意工具名）
    Pattern  string  // 工具特定 glob 模式
    Decision string  // "allow" | "deny" | "ask"
    Scope    string  // "once" | "session" | "permanent"
}
```

`Tool` 字段 SHALL 用 **glob 匹配**（与 `Pattern` 同一套 `MatchGlob` 语法）。不含 glob 元字符（`*` `?` `[`）的值与字面比较等价，既有精确规则语义不变；空字符串与 `"*"` 均匹配任意工具名。MCP 工具按其注册名 `<server>__<tool>` 参与匹配，因此 `dataweave__query_*` 可一次覆盖该 server 下所有 `query_` 前缀工具。

`Pattern` 字段 SHALL 用 **glob 匹配**，语法基于 Go `path/filepath.Match` 扩展：

- `*` 匹配单段内任意字符（不跨 `/`、不跨 `:`）
- `**` 匹配任意多段（含 `/` 与 `:`）
- `?` 匹配单个字符
- 字面字符必须 exact match
- 大小写敏感

每个工具自定义 Pattern 的语义：

| 工具 | Pattern 含义 | 示例 |
|---|---|---|
| `Bash` | 命令字符串（trim 后），与 `Pattern` glob 匹配 | `"git status:*"` 匹配 `git status` / `git status -s`；`"npm *"` 匹配 `npm install` 等 |
| `Read` / `Write` / `Edit` / `Grep` | 主路径参数（如 `Read.path`），与 `Pattern` glob 匹配 | `"/proj/**"` 匹配 `/proj` 及任何子路径；`/proj/*.go` 匹配单层 .go 文件 |
| MCP 工具 / Dispatch / LoadSkill | 工具特定主参数；缺省时 Pattern `*` 匹配所有调用 | - |

`permanent` 作用域 SHALL 持久化到 SQLite `permissions` 表；`session`/`once` 仅在内存。

多条永久规则同时命中时既有优先级不变：`deny_permanent` SHALL 优先于任何 allow，与创建顺序无关——因此 `tool: "dataweave__*" allow_permanent` 与 `tool: "dataweave__node_exec" deny_permanent` 并存时，`node_exec` 被拒绝。

#### Scenario: permanent 规则跨会话生效

- **WHEN** 会话 A 中用户选 `allow_permanent` 同意 `Bash: "git status:*"`，会话 A 结束后新建会话 B
- **THEN** 会话 B 中 LLM 调用 `Bash { command: "git status" }`，不再触发权限询问

#### Scenario: tool glob 免打扰放行 MCP 只读工具

- **WHEN** 存在预设规则 `{ tool: "dataweave__query_*", decision: allow_permanent }`，一次会话内 LLM 连续调用 `dataweave__query_tasks`、`dataweave__query_instances` 等 20 个 `query_` 前缀 MCP 工具
- **THEN** 0 次权限询问、0 个 `permission_request` 事件，全部直接执行

#### Scenario: tool glob 下 deny 仍优先

- **WHEN** 同时存在 `{ tool: "dataweave__*", decision: allow_permanent }` 与 `{ tool: "dataweave__node_exec", decision: deny_permanent }`，LLM 调用 `dataweave__node_exec`
- **THEN** 命中 deny_permanent，返回 `tool_result { is_error: true }`，不执行

#### Scenario: 无元字符规则语义不变

- **WHEN** 既有规则 `{ tool: "Bash", pattern: "git *", decision: allow_permanent }`（tool 不含 glob 元字符）
- **THEN** 其匹配行为与 glob 化之前完全一致：仅匹配工具名恰为 `Bash` 的调用

### Requirement: 权限决策值语义

客户端 POST 的 `permission_decision` 消息的 `decision` 字段 SHALL 接受以下 5 个值：

- `allow_once`：仅本次允许；不持久化
- `allow_session`：本会话内同 tool+pattern 允许；存内存
- `allow_permanent`：跨会话允许；写入 SQLite
- `deny`：仅本次拒绝；不持久化
- `deny_permanent`：跨会话拒绝；写入 SQLite

#### Scenario: allow_session 第二次不再询问

- **WHEN** 用户对 `Bash: "ls *"` 选 `allow_session`，同会话内 LLM 第二次调用 `Bash { command: "ls /tmp" }`
- **THEN** 第二次调用不发 `permission_request` 事件，直接执行

Agent SHALL 在每次工具执行前按以下顺序查询：

1. 检查工具调用是否命中 DangerousCommandGuard（仅 Bash），若命中：**强制询问，绕过所有 allow 规则**
2. 查 session-scope 规则（内存）
3. 查 permanent-scope 规则（SQLite）。当同一调用匹配到多条永久规则时，`deny_permanent` SHALL 优先于 `allow_permanent`，与规则创建顺序无关——使后加（或更具体）的 deny 规则总能收紧一条更早、更宽的 allow（如某条预设）
4. 命中 `allow` → 执行；命中 `deny` → 返回 `tool_result { is_error: true, output: "denied by permission rule" }`
5. 若 `tools.default_permission` 已配置（非空），SHALL 直接返回该决策值，**不弹窗**
6. 未配置 `default_permission` → emit `permission_request` 事件并阻塞等 `permission_decision`

`tools.default_permission` 合法值 SHALL 为 `allow_permanent` 或 `deny_permanent`。空字符串表示不启用（保持现有弹窗行为）。

#### Scenario: deny 规则阻止工具

- **WHEN** 存在规则 `Bash: "rm *" deny session`，LLM 调用 `Bash { command: "rm tmp.txt" }`
- **THEN** Agent 不执行 Bash；返回 `tool_result { is_error: true, output: "denied by permission rule" }`

#### Scenario: deny_permanent 优先于更早的 allow_permanent

- **WHEN** 存在更早创建的 `Bash: "*" allow_permanent`（如预设）与更晚创建的 `Bash: "rm *" deny_permanent`，LLM 调用 `Bash { command: "rm file" }`
- **THEN** Agent 命中 deny_permanent，返回 `tool_result { is_error: true, output: "denied by permission rule" }`；而调用 `Bash { command: "ls" }`（仅 allow 覆盖）仍执行

#### Scenario: 询问超时

- **WHEN** Agent emit `permission_request` 后 `agent.permission_request_timeout_seconds` 秒（默认 300）无 `permission_decision`
- **THEN** Agent 视为 `deny`，返回 `tool_result { is_error: true, output: "permission request timed out" }`

#### Scenario: default_permission 静默放行

- **WHEN** `tools.default_permission: allow_permanent`，LLM 调用 `Read { path: "/tmp/foo.txt" }`，该调用无任何匹配的 session 或 permanent 规则，且非危险命令
- **THEN** Agent 不 emit `permission_request` 事件，直接执行 Read 工具

#### Scenario: default_permission 不覆盖 deny_permanent

- **WHEN** `tools.default_permission: allow_permanent`，存在规则 `Read: "**" deny_permanent`，LLM 调用 `Read { path: "/tmp/foo.txt" }`
- **THEN** Agent 在步骤 3 命中 deny_permanent 规则，返回 `tool_result { is_error: true, output: "denied by permission rule" }`，不执行 Read

#### Scenario: default_permission 不影响危险命令

- **WHEN** `tools.default_permission: allow_permanent`，LLM 调用 `Bash { command: "rm -rf /etc" }`（命中 DangerousCommandGuard）
- **THEN** Agent 仍 emit `permission_request` 事件并阻塞等用户决策

### Requirement: Bash DangerousCommandGuard

Bash 工具 SHALL 在执行前用正则匹配命令内容；命中以下 **8 类**任一模式 SHALL 强制询问（即使存在 `allow_permanent` 规则）：

1. `rm -rf /` / `rm -rf /<path>` / `rm -rf ~`（递归删根/家目录）
2. `dd ... of=/dev/*`（块设备直写）
3. `mkfs.* /dev/*`（块设备格式化）
4. 输出重定向到块设备 `> /dev/(sd|nvme|hd)`
5. fork bomb `:(){ :|:& };:`
6. `chmod -R 777 /`（开放根权限）
7. `shutdown` / `reboot` / `halt` / `poweroff`（系统断电/重启）
8. 命令字符串含可疑解码执行模式（如 `base64 -d | sh`、`curl ... | bash`）

`permission_request` 事件 SHALL 含 `risk: "catastrophic"` 标志。

**已知绕过方式（MVP 接受此风险）**：

正则匹配只能识别字面模式，下列绕过方式 MVP 不防：

- 字符转义/编码：`\x72m -rf /`、`rm -rf /`、十进制/八进制转义
- 路径替代：`/bin/rm -rf /`、绝对路径
- shell 包装：`bash -c "rm -rf /"`、`sh <<EOF\nrm -rf /\nEOF`
- 别名/函数：`alias r=rm; r -rf /`、shell function
- 解码执行：`echo cm0gLXJmIC8K | base64 -d | sh`
- 同形异义字符：用 Cyrillic `а` 替 Latin `a`
- 多余空格：`rm     -rf      /`（已通过正则 `\s+` 处理常见情况）

V2 计划增强：可插拔规则系统、调用 Haiku 做语义分类、shell parser 树形分析。MVP 接受这些限制是因为 workhorse-agent 是本地单用户工具，安全模型基于"信任用户的 prompt"，DangerousCommandGuard 主要防"无心之失"而非"恶意攻击"。这一限制 SHALL 在 README 与 docs/protocol.md 中明示。

#### Scenario: rm -rf 命中防护

- **WHEN** LLM 调用 `Bash { command: "rm -rf /home/u/proj" }`，且存在 `Bash:* allow permanent` 规则
- **THEN** Agent 仍 emit `permission_request { tool: "Bash", risk: "catastrophic", preview: "rm -rf /home/u/proj" }`，等用户决策

#### Scenario: 普通 rm 不命中

- **WHEN** LLM 调用 `Bash { command: "rm tmp.log" }`
- **THEN** 不命中 DangerousCommandGuard，走常规权限链

### Requirement: 权限规则运行期重载即时生效

当 `tools.preset_rules` 或 `tools.default_permission` 经热加载（见 `configuration` 能力）在运行期更新后，服务 SHALL 使其对后续所有 `permission.Manager.Check()` 调用即时生效，**包括正在运行的会话**，无需该会话重启或重连。

- `preset_rules` 的更新经 `applyPresetRules` 幂等对账写入 SQLite 的 `preset-*` 行；由于 `Check()` 在每次调用时实时查询 store 的永久规则，更新后的下一次工具调用即读到新规则。
- `default_permission` 的更新 SHALL 通过 `Manager` 上加锁的 setter 即时替换其内部缓存值，使后续 `Check()` 的兜底决策反映新值。
- 重载 SHALL NOT 影响 `session`/`once` 作用域的内存规则，也 SHALL NOT 触碰由 `/v1/permissions` 创建的 `perm-*` 手动规则。

#### Scenario: 同一会话下个 loop 命中新增 deny

- **WHEN** 某会话正在运行，期间 `config.yaml` 经热加载新增 `{tool: Bash, pattern: "rm *", decision: deny_permanent}`
- **THEN** 同一会话**下一次** `Bash { command: "rm tmp" }` 调用的 `Check()` 命中该 deny，返回拒绝，无需重启会话

#### Scenario: default_permission 运行期切换即时生效

- **WHEN** `serve` 运行中，`tools.default_permission` 由 `""` 改为 `deny_permanent` 并热加载
- **THEN** 后续无匹配规则的工具调用不再弹窗询问，而是按新的 `deny_permanent` 兜底返回拒绝

#### Scenario: 重载不影响会话内已授予的 allow_session

- **WHEN** 会话内用户已对 `Bash: "ls *"` 选择 `allow_session`，随后发生一次不涉及该模式的 preset 热加载
- **THEN** 该会话内 `ls` 调用仍按 `allow_session` 直接放行，内存规则不被重载清除

### Requirement: 权限决议可观察性

权限检查的请求与决议 SHALL 以结构化事件出现在会话 SSE 流中，供外部审批系统与审计落库使用：

- `permission_request` SHALL **仅在**权限检查实际进入 prompt（无规则命中、无 default、或危险升级强制询问）时发射**一次**，payload 含 `request_id`（SHALL 等于触发该检查的 tool_use id）、`tool`、`resource`、`dangerous`、`reason`、`expires_at`（RFC3339，= 发射时刻 + `agent.permission_request_timeout_seconds`）。规则或 `default_permission` 自动决议的调用 SHALL NOT 产生 `permission_request`。
- 每次权限检查结束后服务 SHALL 发射 `permission_resolved`，payload 含 `request_id`（= tool_use id）、`tool`、`decision`（5 级决策值之一）、`source` ∈ `{ "rule", "default", "prompt", "timeout", "none" }`。`timeout` 表示 prompt 超时按 deny 处理；`none` 表示无 prompt 回调可用时的兜底 deny。
- 上述两事件 SHALL 与其他会话事件一样持久化进 `events` 表并参与 `Last-Event-ID` 续传。

#### Scenario: 外部审批闭环

- **WHEN** LLM 调用无任何规则覆盖的 `dataweave__publish_workflow`（tool_use id 为 `toolu_X`），外部系统在超时前 POST `permission_decision { request_id: "toolu_X", decision: "allow_once" }`
- **THEN** SSE 流依次出现 `permission_request { request_id: "toolu_X", expires_at: ... }`、`permission_resolved { request_id: "toolu_X", decision: "allow_once", source: "prompt" }`、`tool_call_start { id: "toolu_X" }`

#### Scenario: 超时决议可观察

- **WHEN** `permission_request` 发射后超过 `agent.permission_request_timeout_seconds` 无应答
- **THEN** SSE 流出现 `permission_resolved { decision: "deny", source: "timeout" }`，随后的 `tool_call` 结果为 `is_error: true`

#### Scenario: 规则放行不产生审批请求

- **WHEN** 预设规则命中使调用自动放行
- **THEN** 无 `permission_request` 事件；SSE 流出现 `permission_resolved { decision: "allow_permanent", source: "rule" }` 后直接 `tool_call_start`

