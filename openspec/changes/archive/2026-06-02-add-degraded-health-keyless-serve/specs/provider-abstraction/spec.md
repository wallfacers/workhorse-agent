## ADDED Requirements

### Requirement: 无可用 provider key 时降级启动

`serve` SHALL 在 default provider 缺少可用 API key 时仍然 bind HTTP 监听并进入
降级(degraded)状态,而非启动失败退出。降级状态下,只有真正需要调用 provider 的
操作(如创建/运行会话)SHALL 返回结构化错误;`/health`、能力查询等不依赖 provider
的端点 SHALL 正常服务。此降级仅适用于"可恢复的缺 key";其它致命配置错误(如非法
枚举值)SHALL 仍然 fail-fast 启动失败,不进入降级态。

#### Scenario: 缺 key 不再阻止 serve 启动

- **WHEN** 启动 `serve`,default provider 既无 config 也无 env 提供 API key
- **THEN** 进程 SHALL NOT 退出
- **AND** SHALL bind 配置的 `host:port` 并接受 HTTP 请求
- **AND** `/health` SHALL 以 `ok: false, reason: "no_provider_key"` 响应

#### Scenario: 降级态下需要 provider 的操作返回明确错误

- **WHEN** 服务处于 `no_provider_key` 降级态,客户端发起需要 provider 的会话运行
- **THEN** 服务 SHALL 返回结构化错误,指明缺少 provider key
- **AND** SHALL NOT 崩溃或静默无响应

#### Scenario: 致命配置错误仍 fail-fast

- **WHEN** 启动 `serve`,配置含致命非法项(如 `tools.default_permission: allow_once`)
- **THEN** 进程 SHALL 启动失败并在 stderr 输出校验错误
- **AND** SHALL NOT 进入降级态
