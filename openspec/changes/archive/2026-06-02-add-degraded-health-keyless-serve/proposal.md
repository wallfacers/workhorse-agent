## Why

上游 `workhorse-assistant` 要新增 **Native 运行时模式**:由 app 在 Windows host 上自托管 `workhorse-agent.exe`(详见 assistant 仓 `add-native-runtime-mode`)。这把"配置 agent"的责任从"用户在终端手动 `init`"转移到了"app 自动拉起进程"。

但当前 `serve` 在 default provider 没有可用 API key 时**直接启动失败**(`cmd/workhorse-agent/cmd_serve.go:355` → `serve: providers.default %q has no API key configured`),且**根本不 bind HTTP 监听**就退出。后果:

```
干净机器,无 key
   ↓
app 注入的 env 还没填好 / 用户尚未配置
   ↓
serve 秒退,不开监听
   ↓
托管方(supervisor)只看到"进程死了"→ 通用 Failed,无法区分"缺 key"与其他崩溃
   ↓
用户在 UI 上看到一片红,无从下手
```

`serve` 已支持无 `config.yaml`、纯环境变量启动(`internal/config/load.go:80-99` 缺文件回落 defaults+env),所以 happy path 不需要改;**缺的是"无 key 时的优雅降级",好让托管方连上后给出精准的"请配置 API key"引导,而不是 crash-loop。**

## What Changes

- **`serve` 在无可用 provider key 时不再启动失败**:仍 bind HTTP 监听并进入**降级(degraded)**状态;只有真正需要 provider 的操作(如创建/运行会话)才返回明确错误。
- **`/health` 暴露降级原因**:当服务可达但不可用时,响应 `{ ok: false, reason: "<machine-readable>" }`,新增 `reason` 机读字段(无 key → `reason: "no_provider_key"`)。`ok:true` 时不含 `reason`,向后兼容。
- **托管方可据此引导**:assistant 连上降级中的 sidecar,读到 `reason: "no_provider_key"`,弹出精准的"填 API key"面板;填好后经 env/热重载恢复 `ok:true`。

> 范围严格限定在"无 key 的托管启动体验"。不改协议传输、不改鉴权、不改 provider 调用语义。

## Capabilities

### Modified Capabilities
- `api-protocol`: `/health` 在 `ok:false` 时新增可选 `reason` 机读字段,用于区分降级原因(首个取值 `no_provider_key`)。
- `provider-abstraction`: `serve` 启动不再因 default provider 缺 key 而失败;改为降级启动,provider 不可用时按需返回结构化错误。

## Impact

- **`cmd/workhorse-agent/cmd_serve.go`**:`buildProviderRegistry` 缺 key 时不再 `return err` 中断 serve;改为记录降级状态,继续启动 HTTP 监听。
- **`/health` handler**(`internal/api/`):降级时返回 `ok:false` + `reason`。
- **会话/运行路径**:用到不可用 provider 时返回明确错误(沿用既有 error 事件 schema)。
- **热重载**:补齐 key(env 或 config 热重载)后,降级状态 SHALL 清除,`/health` 恢复 `ok:true`。
- **上游 assistant**:依赖 `reason: "no_provider_key"` 渲染"配置 API key"引导(`add-native-runtime-mode` 的 Q2)。
