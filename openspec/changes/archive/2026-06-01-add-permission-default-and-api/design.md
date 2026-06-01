## Context

当前权限系统（`permission-control` spec）的决策链为：危险命令拦截 → session 规则 → permanent 规则 → 弹窗。无匹配规则时总是弹窗。`Manager` 不感知任何「默认决策」概念，`prompt` 回调为 nil 时硬编码 deny。

配置系统（`configuration` spec）的 `ToolsConfig` 只有 `default_allowed_tools` 一个权限相关字段，仅控制工具白名单，不涉及决策策略。

权限规则的管理完全依赖弹窗流程 — 没有 HTTP API、没有 CLI、没有启动时注入。用户无法声明「所有 Read 操作永远 allow」，只能逐次点击累积规则。

## Goals / Non-Goals

**Goals:**
- 提供 `default_permission` 配置，让用户设定「无规则匹配时」的静默决策
- 提供 `preset_rules` 配置，启动时自动注入永久规则，声明式管理
- 提供 HTTP API 运行时 CRUD 永久规则，立即生效（每次 Check 查询 SQLite）
- 提供 CLI 子命令操作同一套 API
- Web UI 增加权限管理面板
- 危险命令始终弹窗，不受 default_permission 或 preset_rules 影响
- 完全向后兼容：不配置新字段 = 现有行为不变

**Non-Goals:**
- config.yaml 热加载（项目约定需要重启）
- session 级别规则的 API 管理（session 规则本质是临时的，API 只操作 permanent）
- `allow_once`/`allow_session`/`deny` 作为 default_permission 值（无状态默认无意义）
- preset_rules 支持 session/once 作用域（只支持 permanent）
- 替换 DangerousCommandGuard（不受影响）

## Decisions

### Decision 1: default_permission 放在决策链的位置

**选择**: 步骤 4 — permanent rules 之后、弹窗之前。

**理由**:
- permanent rules（含 deny_permanent）必须优先匹配，否则 `default_permission: allow_permanent` 会覆盖用户显式设置的 deny 规则
- 放在弹窗之前是唯一合理的 fallback 位置：危险命令已强制弹窗，明确规则已匹配，剩下就是「用户没说过怎么办」

**替代方案**: 放在步骤 2 之前（最高优先）→ 拒绝，因为会覆盖 deny_permanent 规则，安全降级。

### Decision 2: Preset rule 的 ID 生成策略

**选择**: `perm-` + hex(md5(tool + "\x00" + pattern + "\x00" + decision))[:16]

**理由**:
- 确定性：同一条配置每次启动生成相同 ID
- `INSERT OR REPLACE` 天然幂等：重新创建同 ID 规则 = 更新 created_at
- 不与手动规则冲突：手动规则用随机 ID (`perm-` + rand 8 bytes)，不同命名空间

**替代方案**: 随机 ID + 去重逻辑 → 拒绝，因为去重逻辑复杂（要判断 tool+pattern+decision 三元组是否已存在），确定性 ID 更简单可靠。

### Decision 3: SavePermission 改为 INSERT OR REPLACE

**选择**: 将 SQL 从 `INSERT INTO` 改为 `INSERT OR REPLACE INTO`。

**理由**: preset rules 每次启动都要「确保存在」。如果用 INSERT 遇到主键冲突会报错，需要额外 SELECT + INSERT/UPDATE 逻辑。INSERT OR REPLACE 一行 SQL 解决。

**风险**: 手动规则和 preset 规则 ID 冲突。由于 preset 用 md5 hex 而手动用 rand hex，命名空间不同（`perm-` 前缀相同但 ID 生成路径隔离），冲突概率可忽略。

**替代方案**: 保持 INSERT，在应用层做 upsert → 拒绝，多一次 round-trip 没有收益。

### Decision 4: API 只操作 permanent 规则

**选择**: API 的 list/create/delete 只操作 `scope=permanent` 的规则。

**理由**:
- session 规则是运行时临时状态，通过弹窗创建，通过 API 暴露会让 session 生命周期管理变复杂
- once 规则天然是一次性的，不需要管理
- 用户的实际需求是「管理永久规则」，session 级别不需要

### Decision 5: CLI 通过 HTTP API 而非直接操作 SQLite

**选择**: CLI 子命令调用 `POST/GET/DELETE /v1/permissions`。

**理由**:
- 与 sessions 子命令模式一致
- 不需要 CLI 持有 store 依赖
- API 是唯一入口点，避免 CLI 绕过 API 直接写 SQLite 导致数据不一致

## Risks / Trade-offs

- [预设规则被 API 删除后重启会恢复] → 这是设计意图: config 是预设规则的 source of truth。如果用户想永久删除，应该从 config.yaml 移除。文档需明示。
- [修改 preset_rules 后重启，旧规则残留] → 接受: 改名/删除 preset rule 后，旧 ID 对应的 DB 行不会被自动清理。可后续加 `--clean-preset` 启动参数或手动 API 删除。
- [default_permission: allow_permanent 过于宽松] → 危险命令不受影响。用户自担风险，配置文档会说明建议值。deny_permanent 也是合法选项。
- [INSERT OR REPLACE 覆盖手动修改] → 手动规则用随机 ID 不会冲突；预设规则的 DB 行被 REPLACE 是预期行为（config 为准）。
