## ADDED Requirements

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
