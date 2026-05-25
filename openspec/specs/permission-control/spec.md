# permission-control Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: 权限规则数据结构与匹配语义

服务 SHALL 维护权限规则集合，每条规则含：

```go
type Permission struct {
    Tool     string  // "Bash" / "Edit" / "*"（通配，匹配任意工具名）
    Pattern  string  // 工具特定 glob 模式
    Decision string  // "allow" | "deny" | "ask"
    Scope    string  // "once" | "session" | "permanent"
}
```

`Tool` 字段 SHALL 用 exact match（`*` 是唯一特殊值，匹配任意工具）。

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

#### Scenario: permanent 规则跨会话生效

- **WHEN** 会话 A 中用户选 `allow_permanent` 同意 `Bash: "git status:*"`，会话 A 结束后新建会话 B
- **THEN** 会话 B 中 LLM 调用 `Bash { command: "git status" }`，不再触发权限询问

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

### Requirement: 权限匹配流程

Agent SHALL 在每次工具执行前按以下顺序查询：

1. 检查工具调用是否命中 DangerousCommandGuard（仅 Bash），若命中：**强制询问，绕过所有 allow 规则**
2. 查 session-scope 规则（内存）
3. 查 permanent-scope 规则（SQLite）
4. 命中 `allow` → 执行；命中 `deny` → 返回 `tool_result { is_error: true, output: "denied by permission rule" }`
5. 未命中 / 命中 `ask` → emit `permission_request` 事件并阻塞等 `permission_decision`

#### Scenario: deny 规则阻止工具

- **WHEN** 存在规则 `Bash: "rm *" deny session`，LLM 调用 `Bash { command: "rm tmp.txt" }`
- **THEN** Agent 不执行 Bash；返回 `tool_result { is_error: true, output: "denied by permission rule" }`

#### Scenario: 询问超时

- **WHEN** Agent emit `permission_request` 后 `agent.permission_request_timeout_seconds` 秒（默认 300）无 `permission_decision`
- **THEN** Agent 视为 `deny`，返回 `tool_result { is_error: true, output: "permission request timed out" }`

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

