## ADDED Requirements

### Requirement: 运行期权限配置热加载

`serve` 运行期间，服务 SHALL 监听 `~/.workhorse-agent/config.yaml` 的变更并在变更后热加载**权限子集**，无需重启进程，且 SHALL NOT 中断正在运行的会话。

监听 SHALL 满足：

- 监听 `config.yaml` 所在**目录**（而非固定 inode），以正确捕获编辑器"写临时文件 + rename"式存盘；
- 对短时间内的连续写入事件 SHALL 做 debounce 合并（如 200ms 窗口），避免半成品文件触发重载；
- SHALL 额外接受 `SIGHUP` 信号作为手动重载触发入口。

热加载触发后，服务 SHALL 以与启动相同的逻辑重新执行 `config.Load()`（含校验）。

可热加载的字段集合 SHALL 限定为：`tools.preset_rules`、`tools.default_permission`、`agent.permission_request_timeout_seconds`。其余字段（包括但不限于 `store.path`、`server.host`、`server.port`、`providers.*`）SHALL NOT 在运行期热换。

#### Scenario: 手改 preset_rules 后热加载生效

- **WHEN** `serve` 运行中，用户编辑 `config.yaml` 在 `tools.preset_rules` 新增一条合法规则并保存
- **THEN** 服务在 debounce 窗口后重载，将该规则对账进 SQLite，且进程不重启、已有会话不中断

#### Scenario: SIGHUP 触发重载

- **WHEN** `serve` 运行中，进程收到 `SIGHUP`
- **THEN** 服务重新执行 `config.Load()` 并应用权限子集变更

#### Scenario: 非权限字段变更被忽略并告警

- **WHEN** 热加载时检测到 `server.port` 或 `store.path` 较当前生效值发生变化
- **THEN** 服务 SHALL NOT 改变监听端口或数据库路径，并记录一条 `WARN` 日志说明该字段需重启方可生效

### Requirement: 热加载校验闸门

热加载重新执行 `config.Load()` 时，若解析失败或校验（validate）未通过，服务 SHALL 保留当前已生效的配置，记录 `WARN` 日志，并 SHALL NOT 应用任何来自该次失败重载的字段。

#### Scenario: 写坏 YAML 不影响当前配置

- **WHEN** `serve` 运行中，用户将 `config.yaml` 改成语法错误或含非法 `decision` 值的内容并保存
- **THEN** 服务记录 `WARN`，继续使用变更前已生效的权限规则，已有会话的 `Check()` 行为不变

#### Scenario: 校验通过后才应用

- **WHEN** 用户先存了一份语法错误的文件（被拒绝），随后修正为合法内容再次保存
- **THEN** 第二次保存通过校验，新的权限子集被应用并对账进 SQLite
