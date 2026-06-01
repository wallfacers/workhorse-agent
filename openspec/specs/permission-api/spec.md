# permission-api Specification

## Purpose
TBD - created by archiving change add-permission-default-and-api. Update Purpose after archive.
## ADDED Requirements

### Requirement: 列出所有永久权限规则

服务 SHALL 提供 `GET /v1/permissions` 端点，返回所有 `scope=permanent` 的权限规则列表。

每条规则 SHALL 包含以下字段：

```json
{
  "id": "preset-a1b2c3d4e5f6g7h8",
  "session_id": "",
  "tool": "Bash",
  "pattern": "git *",
  "decision": "allow_permanent",
  "scope": "permanent",
  "source": "preset",
  "created_at": "2026-06-01T10:00:00Z"
}
```

`source` 字段 SHALL 按规则 ID 前缀判定：以 `preset-` 开头为 `"preset"`，否则为 `"manual"`（手动创建的规则使用 `perm-` 前缀，二者永不冲突）。

空列表时 SHALL 返回 `200` 含 `{"rules": []}`。

#### Scenario: 返回混合来源的规则列表

- **WHEN** 数据库中存在 2 条永久规则：一条 preset（ID 为确定性 ID）、一条手动（ID 为随机 hex）
- **THEN** 返回 `200`，`rules` 数组含 2 条，`source` 分别为 `"preset"` 和 `"manual"`

#### Scenario: 无规则时返回空列表

- **WHEN** 数据库中无任何永久规则
- **THEN** 返回 `200`，`{"rules": []}`

---

### Requirement: 创建永久权限规则

服务 SHALL 提供 `POST /v1/permissions` 端点，接受 JSON body 创建一条永久权限规则。

请求体 SHALL 接受以下字段：

```json
{
  "tool": "Bash",
  "pattern": "npm run build",
  "decision": "allow_permanent"
}
```

- `tool`：可选。空字符串或 `"*"` 表示匹配所有工具
- `pattern`：可选。空字符串表示匹配所有 resource（等价 `"**"`）
- `decision`：必选。合法值为 `allow_permanent` 或 `deny_permanent`

服务 SHALL 使用随机 8 字节 hex 生成 ID（格式 `perm-<hex>`），`session_id` 固定为 `""`（全局规则），`scope` 固定为 `permanent`。

成功时 SHALL 返回 `201` 含创建出的完整 rule 对象。

`decision` 字段非法时 SHALL 返回 `400` 含 `{"error": "invalid decision: <value>"}`。

#### Scenario: 成功创建规则

- **WHEN** POST `/v1/permissions` body `{"tool":"Bash","pattern":"npm *","decision":"allow_permanent"}`
- **THEN** 返回 `201`，body 含 `id`、`tool: "Bash"`、`pattern: "npm *"`、`decision: "allow_permanent"`、`scope: "permanent"`、`source: "manual"`

#### Scenario: 创建规则后立即生效

- **WHEN** POST 创建 `{"tool":"Read","pattern":"/tmp/**","decision":"allow_permanent"}` 返回 `201`
- **THEN** 同一会话中下一次 `Read { path: "/tmp/foo.txt" }` 不再弹窗（permanent 规则在步骤 3 命中）

#### Scenario: 非法 decision 拒绝

- **WHEN** POST `/v1/permissions` body `{"decision":"allow_once"}`
- **THEN** 返回 `400`，`{"error": "invalid decision: allow_once"}`

---

### Requirement: 删除永久权限规则

服务 SHALL 提供 `DELETE /v1/permissions/{id}` 端点，删除指定 ID 的永久规则。

成功时 SHALL 返回 `204 No Content`。ID 不存在时 SHALL 返回 `404` 含 `{"error": "not found"}`。底层存储错误等其它失败 SHALL 返回 `500`,不得将其混淆为 `404`。

删除预设规则 SHALL 被允许；被删除的预设规则在下次服务重启时 SHALL 被重新创建。

#### Scenario: 成功删除规则

- **WHEN** DELETE `/v1/permissions/perm-a1b2c3d4e5f6g7h8` 且该规则存在
- **THEN** 返回 `204`，规则从数据库移除；后续同 tool+pattern 调用按默认策略处理

#### Scenario: 删除不存在的规则

- **WHEN** DELETE `/v1/permissions/perm-nonexist`
- **THEN** 返回 `404`，`{"error": "not found"}`
