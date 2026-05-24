## ADDED Requirements

### Requirement: 权限规则数据结构

服务 SHALL 维护权限规则集合，每条规则含：

```go
type Permission struct {
    Tool     string  // "Bash" / "Edit" / "*"（通配）
    Pattern  string  // 工具特定匹配，如 Bash: "git status:*"; Edit: "/path/glob/**"
    Decision string  // "allow" | "deny" | "ask"
    Scope    string  // "once" | "session" | "permanent"
}
```

`permanent` 作用域 SHALL 持久化到 SQLite `permissions` 表；`session`/`once` 仅在内存。

#### Scenario: permanent 规则跨会话生效

- **WHEN** 会话 A 中用户选 `allow_permanent` 同意 `Bash: "git status:*"`，会话 A 结束后新建会话 B
- **THEN** 会话 B 中 LLM 调用 `Bash { command: "git status" }`，不再触发权限询问

### Requirement: 权限决策值语义

WebSocket `permission_decision` 消息的 `decision` 字段 SHALL 接受以下 5 个值：

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

- **WHEN** Agent emit `permission_request` 后 5 分钟（可配）无 `permission_decision`
- **THEN** Agent 视为 `deny`，返回 `tool_result { is_error: true, output: "permission request timed out" }`

### Requirement: Bash DangerousCommandGuard

Bash 工具 SHALL 在执行前用正则匹配命令内容；命中以下任一模式 SHALL 强制询问（即使存在 `allow_permanent` 规则）：

- `rm -rf /` 或 `rm -rf /<path>`、`rm -rf ~`
- `dd ... of=/dev/*`
- `mkfs.* /dev/*`
- 重定向 `> /dev/(sd|nvme|hd)`
- fork bomb `:(){ :|:& };:`
- `chmod -R 777 /`
- `shutdown` / `reboot` / `halt` / `poweroff`

`permission_request` 事件 SHALL 含 `risk: "catastrophic"` 标志。

#### Scenario: rm -rf 命中防护

- **WHEN** LLM 调用 `Bash { command: "rm -rf /home/u/proj" }`，且存在 `Bash:* allow permanent` 规则
- **THEN** Agent 仍 emit `permission_request { tool: "Bash", risk: "catastrophic", preview: "rm -rf /home/u/proj" }`，等用户决策

#### Scenario: 普通 rm 不命中

- **WHEN** LLM 调用 `Bash { command: "rm tmp.log" }`
- **THEN** 不命中 DangerousCommandGuard，走常规权限链
