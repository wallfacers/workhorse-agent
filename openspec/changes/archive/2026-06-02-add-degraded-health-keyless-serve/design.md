## Context

`workhorse-assistant` 新增 Native 运行时(app 自托管 `workhorse-agent.exe`)。当前 `serve` 启动链:

```
runServe → config.Load(缺 yaml 回落 defaults+env, load.go:80-99) ✅
        → buildProviderRegistry(cfg)
             若 default provider 无 key → return error (cmd_serve.go:355) ❌ serve 整体失败,不 bind 监听
```

env 注入已可用(`WORKHORSE_AGENT_PROVIDERS_ANTHROPIC_API_KEY` 等,load.go:154-166)。问题仅在"key 尚未就位"这个时间窗:托管方拿不到一个"活着但缺 key"的服务,只能看到进程死亡。

`/health` 现状(api-protocol spec):`{ ok, version, uptime_sec, sessions_active, protocol_version, capabilities }`,无机读的失败原因。

## Goals / Non-Goals

**Goals:**

- 无可用 provider key 时,`serve` 仍 bind 监听并进入降级态,不退出。
- `/health` 用机读 `reason` 区分降级原因,托管方据此给精准引导。
- 补齐 key 后经重启恢复(`config.yaml` 不热重载,见 D3)。

**Non-Goals:**

- 不改 `init` 为非交互(env 路径已够,降级态覆盖"还没配"的窗口)。
- 不改协议传输 / 鉴权 / provider 调用语义。
- 不在 agent 侧做任何 UI(引导是 assistant 的事)。
- 不放宽其它致命配置校验(如非法 `default_permission` 仍应启动失败)。

## Decisions

### D1: 降级启动,而非启动失败

**理由:** 托管方需要一个"可连、可问诊"的端点。进程秒退使 supervisor 无法区分"缺 key"与"崩溃"。

**做法:** `buildProviderRegistry` 缺 default key 时不 `return err`,而是返回一个"无可用 provider"的注册表 + 降级标志;`serve` 继续 bind 监听。仅当请求真正需要 provider 时才报错。

**取舍:** 区分"可降级"(缺 key)与"不可降级"(如非法枚举值)两类配置问题——后者仍 fail-fast,不混入降级态。

### D2: `/health` 新增可选 `reason`,仅在 `ok:false` 出现

**理由:** 机读、可扩展、向后兼容。`ok:true` 不变。

**形态:**
```json
{ "ok": false, "reason": "no_provider_key", "version": "...", "uptime_sec": 1,
  "sessions_active": 0, "protocol_version": "1", "capabilities": [...] }
```
`reason` 取值受控枚举,本期仅 `no_provider_key`,未来可加(如 `provider_unreachable`)。HTTP 状态码保持 `200`(健康检查可达即 200,可用性看 `ok`)。

### D3: 补 key 后经重启恢复(config.yaml 不热重载)

**理由:** `config.yaml` 在本项目**不热重载**(CLAUDE.md / `配置热重载范围`:仅 `agents/*.yaml`、`skills/*/skill.yaml` 动态重扫)。因此降级标志在启动时一次性确定,恢复路径是重启。

**做法:** 托管方在 UI 收到 key 后,把它作为 `WORKHORSE_AGENT_PROVIDERS_*` env(或写入 `config.yaml`)注入,然后**重启** sidecar 进程;重启后 `buildProviderRegistry` 重新评估,降级标志清除,`/health` 回 `ok:true`。这与 supervisor 的"切换/重启即 reap+respawn"模型一致,不引入新的热重载机制。

## Risks / Trade-offs

- **R1 降级态被误当健康**:`/health` 返回 200 但 `ok:false`。缓解:协议明确"可用性看 `ok` 非 HTTP 码";assistant 端已读 `ok`。
- **R2 降级与致命错误混淆**:必须只对"可恢复的缺 key"降级,其它配置错误仍 fail-fast(D1 取舍)。
- **R3 reason 取值漂移**:assistant 硬编码字符串。缓解:受控枚举 + 在 api-protocol spec 固化取值表。

## Open Questions

- 除 `no_provider_key` 外,本期是否还要覆盖 `provider_unreachable`(key 有但网络/鉴权失败)?倾向**本期只做 `no_provider_key`**,其余后续按需加。
- 恢复一律走重启(`config.yaml` 不热重载),托管方负责重起进程。未来若要免重启,需新增"provider 注册表热重建"能力,本期不做。
