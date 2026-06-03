## 1. 依赖与脚手架

- [x] 1.1 `go get github.com/fsnotify/fsnotify`，更新 `go.mod`/`go.sum`
- [x] 1.2 新增 `internal/config/reload.go`（重载入口与字段 diff 分类）与 `internal/config/yamledit.go`（yaml.v3 Node 外科式读写）骨架

## 2. Manager 运行期可变（TDD）

- [x] 2.1 写测试：`SetDefaultDecision`/`SetTimeout` 后 `Check()` 反映新值；并发读写无 data race（`-race`）
- [x] 2.2 在 `internal/permission/manager.go` 新增加锁 `SetDefaultDecision(Decision)` 与 `SetTimeout(time.Duration)`，`Check()` 读这两值时取锁（或改 `atomic.Value`）

## 3. 配置 YAML 外科式读写（TDD）

- [x] 3.1 写测试：对带注释的样例 config.yaml，读出 `default_permission`+`preset_rules`；写回新 `preset_rules` 后，`server:`/`auth:` 段注释、空行、键顺序保持不变
- [x] 3.2 实现 `yamledit.ReadPermissionConfig(path)`：解析为 `yaml.Node`，提取 `tools.preset_rules`/`tools.default_permission`；文件缺失返回默认值
- [x] 3.3 实现 `yamledit.WritePermissionConfig(path, cfg)`：定位并替换/插入 `tools` 下两个子节点，`Encode` 后经临时文件 + `os.Rename` 原子写入

## 4. 热加载机制（TDD）

- [x] 4.1 写测试：发 `SIGHUP` 后重载，新增的合法 preset 规则对账进 store（用 in-memory/临时 sqlite 断言 `ListPermissions`）
- [x] 4.2 写测试：写入非法 YAML/非法 decision 后重载，旧规则保留、记 WARN，`Check()` 行为不变
- [x] 4.3 写测试：`server.port` 变更被忽略并产生 WARN（不重绑监听）
- [x] 4.4 实现 `config.Reload`：重跑 `config.Load()`，失败则保留旧配置 + WARN；成功则 diff 出权限子集变化并回调应用（`applyPresetRules` + Manager setter），非权限字段变化 WARN
- [x] 4.5 在 `cmd_serve.go` 接线：启动 fsnotify watcher 监听 `~/.workhorse-agent/` 目录 + 200ms debounce；注册 `SIGHUP` → 触发 `Reload`；与 serve ctx 绑定，优雅关停时 `watcher.Close()`

## 5. 权限配置 HTTP 端点（TDD）

- [x] 5.1 写测试：`GET /v1/permission-config` 返回文件内权限段；文件缺失返回默认值
- [x] 5.2 写测试：`PUT /v1/permission-config` 合法 body 写入并保留注释；非法 decision 返回 400 且不改文件
- [x] 5.3 实现 `GET`/`PUT` handler（`internal/api/permission_config.go`），复用 `yamledit`，PUT 先做白名单校验
- [x] 5.4 在 `internal/api/server.go` 注册路由 `/v1/permission-config`

## 6. 端到端与收尾

- [x] 6.1 集成测试：`PUT` 写入新 deny 规则 → 等热加载 → 同一运行中会话下个 `Check()` 命中该 deny（验证"下个 loop 生效不中断"）
- [x] 6.2 `default_permission` 由空改为 `deny_permanent` 经热加载后，无匹配规则的调用按新兜底拒绝
- [x] 6.3 更新 `cmd/workhorse-agent` 相关文档/CLAUDE.md（如有配置说明）：补充热加载与 `/v1/permission-config` 端点
- [x] 6.4 `golangci-lint` + `go test ./... -race` 全绿
