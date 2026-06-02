## ADDED Requirements

### Requirement: /health 降级原因字段

`GET /health` SHALL 在服务可达但不可用时返回 `ok: false` 并附带一个机读的
`reason` 字段,用于让客户端区分降级原因。`reason` SHALL 取自受控枚举(本期至少
含 `no_provider_key`)。当 `ok: true` 时响应 SHALL NOT 包含 `reason`(向后兼容,
既有字段不变)。无论 `ok` 取值,健康检查 HTTP 状态码 SHALL 保持 `200`——可用性
由 `ok` 字段而非 HTTP 码表达。

#### Scenario: 缺 provider key 时健康检查降级

- **WHEN** 服务在 default provider 无可用 API key 的情况下启动并已 bind 监听,客户端 GET `/health`
- **THEN** 服务返回 `200 OK` 和 `{ "ok": false, "reason": "no_provider_key", "version": "<semver>", "uptime_sec": <int>, "sessions_active": <int>, "protocol_version": "<string>", "capabilities": [<string>, ...] }`

#### Scenario: 健康时不含 reason

- **WHEN** 服务正常可用,客户端 GET `/health`
- **THEN** 响应 SHALL 包含 `ok: true`
- **AND** 响应 SHALL NOT 包含 `reason` 字段

#### Scenario: 补齐 key 后重启恢复健康

- **WHEN** 服务处于 `no_provider_key` 降级态,补齐 default provider 的 key(env 或 `config.yaml`)后重启 serve(`config.yaml` 不热重载,需重启)
- **THEN** 重启后的 `GET /health` SHALL 返回 `ok: true` 且不含 `reason`
