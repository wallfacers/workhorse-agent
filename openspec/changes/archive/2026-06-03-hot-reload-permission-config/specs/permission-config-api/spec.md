## ADDED Requirements

### Requirement: 读取权限配置段

服务 SHALL 暴露 `GET /v1/permission-config` 端点，返回当前 `~/.workhorse-agent/config.yaml` 中权限相关字段的结构化视图：

```json
{
  "default_permission": "",
  "preset_rules": [
    { "tool": "Bash", "pattern": "git *", "decision": "allow_permanent" }
  ]
}
```

`default_permission` SHALL 为 `""`、`allow_permanent` 或 `deny_permanent` 之一。`preset_rules` SHALL 为数组，元素含 `tool`、`pattern`、`decision` 三字段，顺序与文件中一致。

该端点 SHALL 直接读取 `config.yaml`（真源），不读取 SQLite，以反映文件当前内容（含尚未热加载的手改）。当 `config.yaml` 不存在时 SHALL 返回内置默认值（空 `default_permission`、空 `preset_rules`）。

#### Scenario: 读取返回文件内的权限段

- **WHEN** `config.yaml` 的 `tools.preset_rules` 含一条 `{tool: Bash, pattern: "git *", decision: allow_permanent}`
- **THEN** `GET /v1/permission-config` 返回 200，body 的 `preset_rules` 含该条且字段一致

#### Scenario: 配置文件缺失返回默认值

- **WHEN** `config.yaml` 不存在
- **THEN** `GET /v1/permission-config` 返回 200，`default_permission` 为 `""`、`preset_rules` 为空数组

### Requirement: 写入权限配置段（保留注释）

服务 SHALL 暴露 `PUT /v1/permission-config` 端点，接受与 `GET` 相同结构的 body，将 `default_permission` 与 `preset_rules` 写回 `config.yaml` 的 `tools` 段。

写入 SHALL 使用保留注释的方式（YAML node 级编辑），**仅替换 `tools.preset_rules` 与 `tools.default_permission` 两个键**，文件中其余键、注释、空行、字段顺序 SHALL 保持不变。写入 SHALL 为原子操作（写临时文件后 rename）。

请求体中每条 preset 规则的 `decision` SHALL 仅接受 `allow_permanent` 或 `deny_permanent`；`default_permission` SHALL 仅接受 `""`、`allow_permanent`、`deny_permanent`。非法值 SHALL 返回 `400 Bad Request` 并在 body 中给出 `error` 说明与合法取值。

写入成功 SHALL 返回 `200`，并依赖热加载机制使变更生效（见 `configuration` 能力）；该端点本身 SHALL NOT 直接写 SQLite 或调用 `/v1/permissions`。

#### Scenario: 写入仅改 tools 段并保留注释

- **WHEN** `config.yaml` 含带注释的 `server:`、`auth:` 段与一个 `tools:` 段，客户端 `PUT /v1/permission-config` 提交新的 `preset_rules` 列表
- **THEN** 端点返回 200，`config.yaml` 的 `tools.preset_rules` 被替换为新列表，而 `server:`/`auth:` 段及其注释、原有空行与字段顺序保持不变

#### Scenario: 非法 decision 被拒绝

- **WHEN** 客户端 `PUT` 的某条规则 `decision` 为 `allow_session`
- **THEN** 端点返回 400，body 含 `error` 与合法取值列表（`allow_permanent`、`deny_permanent`），`config.yaml` 不被修改

#### Scenario: 写入后经热加载生效

- **WHEN** 客户端 `PUT` 成功新增一条 `{tool: Read, pattern: "/etc/**", decision: deny_permanent}`
- **THEN** 写入触发的 config.yaml 变更经热加载对账进 SQLite，随后任意会话对 `Read /etc/passwd` 的 `Check()` 命中该 deny 规则
