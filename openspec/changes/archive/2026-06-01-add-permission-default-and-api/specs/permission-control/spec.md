## MODIFIED Requirements

### Requirement: 权限匹配流程

Agent SHALL 在每次工具执行前按以下顺序查询：

1. 检查工具调用是否命中 DangerousCommandGuard（仅 Bash），若命中：**强制询问，绕过所有 allow 规则**
2. 查 session-scope 规则（内存）
3. 查 permanent-scope 规则（SQLite）
4. 命中 `allow` → 执行；命中 `deny` → 返回 `tool_result { is_error: true, output: "denied by permission rule" }`
5. 若 `tools.default_permission` 已配置（非空），SHALL 直接返回该决策值，**不弹窗**
6. 未配置 `default_permission` → emit `permission_request` 事件并阻塞等 `permission_decision`

`tools.default_permission` 合法值 SHALL 为 `allow_permanent` 或 `deny_permanent`。空字符串表示不启用（保持现有弹窗行为）。

#### Scenario: deny 规则阻止工具

- **WHEN** 存在规则 `Bash: "rm *" deny session`，LLM 调用 `Bash { command: "rm tmp.txt" }`
- **THEN** Agent 不执行 Bash；返回 `tool_result { is_error: true, output: "denied by permission rule" }`

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
