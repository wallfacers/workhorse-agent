## Why

当前权限系统在无匹配规则时必须弹窗等待用户决策，日常开发中频繁打断工作流。用户需要：一个可配置的默认决策（如 `allow_permanent`）让常规操作静默放行；一套 HTTP API + CLI 在运行时动态管理规则；通过配置文件预设初始规则，启动即生效，无需逐条手动创建。

## What Changes

- 新增 `tools.default_permission` 配置项，无规则匹配时静默返回该决策（不弹窗），为空则保持现有弹窗行为
- 新增 `tools.preset_rules` 配置项，启动时幂等注入 SQLite，支持声明式定义永久规则
- 新增 `GET/POST/DELETE /v1/permissions` HTTP API，运行时 CRUD 永久权限规则
- 新增 `workhorse-agent permissions list/add/remove` CLI 子命令，通过 HTTP API 管理规则
- 修改 `Manager.Check()` 决策链：在 permanent rules 之后、弹窗之前插入 `default_permission` fallback
- `SavePermission` 从 `INSERT` 改为 `INSERT OR REPLACE`，保证 preset rules 重启幂等
- Web UI 新增权限管理面板（表格 + 添加表单 + 删除按钮）
- 危险命令拦截（DangerousCommandGuard）不受影响，始终强制弹窗

## Capabilities

### New Capabilities

- `permission-api`: HTTP CRUD 接口 — `GET /v1/permissions` 列出规则、`POST /v1/permissions` 创建规则、`DELETE /v1/permissions/{id}` 删除规则。规则含 `source` 字段区分 preset/manual。
- `permission-preset`: 启动时从 `tools.preset_rules` 配置读取规则列表，使用确定性 ID (`perm-<md5-hex>`) 通过 `INSERT OR REPLACE` 写入 SQLite，保证重启幂等。

### Modified Capabilities

- `permission-control`: 决策链新增步骤 4 — `default_permission` fallback。当步骤 1-3（危险命令、session 规则、permanent 规则）均未命中时，若 `default_permission` 已配置，静默返回该决策，不弹窗。
- `configuration`: `ToolsConfig` 新增 `default_permission`（string）和 `preset_rules`（[]PresetRule）字段，含校验逻辑。

## Impact

- `internal/config/config.go` — ToolsConfig + PresetRule 类型
- `internal/config/validate.go` — 新字段校验
- `internal/permission/manager.go` — Check() fallback + New() 签名
- `internal/store/sqlite/crud.go` — INSERT → INSERT OR REPLACE
- `internal/api/server.go` — 3 个新 route
- `internal/api/permissions.go` — 新文件：handler 实现
- `cmd/workhorse-agent/cmd_serve.go` — applyPresetRules + 传参
- `cmd/workhorse-agent/cmd_perm.go` — 新文件：CLI 子命令
- `web/index.html` + `web/app.js` — 权限管理面板
