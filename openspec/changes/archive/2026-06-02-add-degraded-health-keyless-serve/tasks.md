## 1. 降级启动(provider 注册表)

- [x] 1.1 `cmd_serve.go` `buildProviderRegistry`:default provider 缺 key 时不 `return err`,改为返回降级原因 `"no_provider_key"`(签名加 `degraded string` 返回值)
- [x] 1.2 区分"可降级缺 key" vs "致命配置错误":后者(如非法 `default_permission`)仍在 `internal/config/validate.go` fail-fast,未触及
- [x] 1.3 `serve` 在降级时记录 WARN 并继续 bind 监听,不退出(`runServe` 捕获 `degraded`,注入 `apiCfg.DegradedReason`)

## 2. /health 降级原因

- [x] 2.1 `internal/api/health.go` handleHealth:降级时返回 `ok:false` + `reason`(机读,本期 `no_provider_key`),HTTP 仍 200
- [x] 2.2 `ok:true` 时不输出 `reason`(向后兼容);`api.Config` 新增 `DegradedReason` 字段
- [x] 2.3 新增 `internal/api/degraded_test.go`:覆盖降级 / 健康两态

## 3. 降级态下的请求语义

- [x] 3.1 `handleCreateSession` 降级守卫:返回 `503 { code: "no_provider_key", message }`(会话不可创建即不可运行,单一卡点)
- [x] 3.2 不依赖 provider 的端点(/health、能力查询)正常服务(已验证 /health 200)

## 4. 恢复(重启,config.yaml 不热重载)

- [x] 4.1 降级标志在启动时由 `buildProviderRegistry` 一次性确定;补 key 后**重启** serve 即重新评估恢复 `ok:true`(`config.yaml` 不热重载,见 design D3)
- [x] 4.2 文档说明:env 注入或改 config 后均需托管方重启进程生效(见 proposal Impact)

## 5. 文档与协议固化

- [x] 5.1 在 change 的 api-protocol spec 登记 `reason` 受控枚举(本期 `no_provider_key`)
- [ ] 5.2 更新 README / 配置文档:无 key 时的降级行为与恢复方式(待主仓文档归档时一并处理)

## 6. 验证

- [x] 6.1 单测:缺 key → serve 启动成功、/health 降级、创建会话 503(`degraded_test.go`)
- [x] 6.2 单测:有 key → /health `ok:true` 不含 reason(`TestHealth_HealthyOmitsReason`)
- [x] 6.3 端到端手动:无 key 启动 → /health `ok:false,reason`、POST 503、进程不崩溃;有 key 启动 → `ok:true` 无 reason(已在本机 7931/7932 验证)
