## Why

权限规则（`tools.preset_rules`、`tools.default_permission`）目前只在 `serve` 启动时从 `~/.workhorse-agent/config.yaml` 读取一次并对账进 SQLite。任何修改——无论手改文件还是未来由 workhorse-assistant 设置页修改——都必须**重启 sidecar** 才能生效，而重启会中断所有正在运行的会话。

用户的诉求是：改完一条权限规则**不打断正在跑的会话**，且**下一个 agent loop（哪怕是同一个 session 的下一次工具调用）立即生效**。同时希望 `config.yaml` 是唯一真源，远程模式下 assistant 也能读写该文件。

## What Changes

- **新增 config.yaml 热加载**：`serve` 运行期间监听 `~/.workhorse-agent/` 目录变更（应对编辑器 rename 存盘），debounce 合并连写，并接受 `SIGHUP` 作为手动触发。检测到变更后重新执行 `config.Load()`（含校验）。
- **校验闸门（fail-safe）**：重载时若解析或校验失败，**保留当前生效的配置**、记录 `WARN` 日志，绝不应用半成品配置（满足"格式对才生效"）。
- **仅热加载权限子集**：重载只应用 `tools.preset_rules`（重跑现有 `applyPresetRules` 幂等对账 SQLite 的 `preset-*` 行）、`tools.default_permission`、`agent.permission_request_timeout_seconds`。其余字段（`store.path`、`server.host/port`、providers 等）即使变更也**不热换**，仅记录 `WARN("需重启生效")`。
- **不中断、下个 loop 生效**：preset 规则经 `applyPresetRules` 写入 SQLite 后，`permission.Manager.Check()` 每次实时读 store，正在运行的会话在下一次工具调用即读到新规则；`default_permission`/超时通过新增的加锁 setter 即时更新。
- **新增"权限配置读写"HTTP 端点**：暴露 `GET`/`PUT` 端点读取与写入 `config.yaml` 的权限段，写入使用 Go `yaml.v3` Node API 做外科式编辑（仅改 `tools` 下相关字段，**保留文件其余注释与内容**）。供 workhorse-assistant 在 native/WSL/远程三种模式下统一读写真源。
- **不引入第二真源**：写端点只改 `config.yaml`，依赖热加载使其生效；不向 `/v1/permissions`（state.db `perm-*`）写入。

## Capabilities

### New Capabilities
- `permission-config-api`: 通过 HTTP 读取/写入 `config.yaml` 的权限配置段（`tools.preset_rules` 与 `tools.default_permission`），写入保留注释，由热加载使其生效。

### Modified Capabilities
- `configuration`: 新增运行期热加载要求——监听 config.yaml 变更、校验闸门、仅重载权限子集、非热加载字段变更记 WARN。
- `permission-control`: 新增要求——`preset_rules`/`default_permission` 在运行期重载后，对包括正在运行的会话在内的后续 `Check()` 即时生效，无需重启。

## Impact

- **代码**：`cmd/workhorse-agent/cmd_serve.go`（接线 watcher/SIGHUP、重载回调）、`internal/config`（重载入口、字段 diff 分类）、`internal/permission/manager.go`（新增加锁 `SetDefaultDecision`/`SetTimeout`）、`internal/api`（新增权限配置读写 handler + 路由）、新增配置 YAML 外科式写入模块。
- **依赖**：新增 `github.com/fsnotify/fsnotify`。
- **API**：新增 2 个 HTTP 端点（读/写权限配置段），路径与协议在 specs 中定义。
- **下游**：workhorse-assistant 的 `permission-settings-page` change 依赖本 change 的新端点。
- **行为**：`serve` 新增长期运行的文件监听 goroutine 与 `SIGHUP` 处理；优雅关停路径需一并停止 watcher。
