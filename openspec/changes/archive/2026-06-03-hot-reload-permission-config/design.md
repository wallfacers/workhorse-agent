## Context

权限规则当前仅在 `cmd_serve.go` 启动时读取一次：`config.Load()` 后 `applyPresetRules(ctx, st, cfg.Tools.PresetRules)` 把 `preset_rules` 幂等对账进 SQLite，`permission.New(..., timeout, defaultDecision)` 把 `default_permission` 与超时**按值烤进** Manager 字段。`cfg` 之后散给约 20 个子系统，无中央可热换的配置持有者。

关键既有事实（决定了方案可行）：
- `Manager.Check()` 第 3 步 `matchPermanentRule` 每次都调 `store.ListPermissions()` **实时读 SQLite**（`internal/permission/manager.go`），不缓存永久规则。
- `applyPresetRules` 用 `presetRuleID = "preset-" + sha256(tool+pattern)[:16]` 生成 ID，`INSERT OR REPLACE` + 删除配置中已移除的 `preset-*`；**只动 `preset-*`，从不碰 `perm-*`**（`cmd_serve.go`）。
- `config.Load()` 内部已含 `validate`，解析/校验失败会返回 error。
- 现有信号仅处理 `SIGTERM/SIGINT`（优雅关停），`SIGHUP` 空闲。
- 无 `fsnotify` 依赖。

## Goals / Non-Goals

**Goals:**
- 运行期修改 `config.yaml` 的权限子集后，不重启、不中断会话，下个 loop 即生效。
- 校验失败时 fail-safe：保留旧配置。
- 暴露 HTTP 端点读/写 `config.yaml` 权限段，保留注释，供 assistant 在 native/WSL/远程统一使用。
- `config.yaml` 是唯一真源，不引入第二写入源。

**Non-Goals:**
- 不做整份配置热加载；`store.path`、`server.*`、providers 等变更仍需重启。
- 不改 `/v1/permissions`（state.db `perm-*` CRUD）的现有语义；本 change 不通过它写入。
- 不实现配置版本协商 / 多文件 include / 远程文件锁等。
- 不在 agent 侧做权限规则的 UI（属 assistant change）。

## Decisions

### D1：监听机制 — fsnotify 监听目录 + debounce + SIGHUP
监听 `~/.workhorse-agent/` **目录**而非 `config.yaml` 文件本身，因为编辑器与原子写入（含本 change 自己的 PUT）都用"写临时文件 + rename"，监听固定 inode 会在 rename 后丢失后续事件。对窗口内（默认 200ms）的连续事件 debounce 合并，只在静默后触发一次重载，规避读到半成品。

额外注册 `SIGHUP` 作为手动触发：既是无文件监听环境（少数 FS 不支持 inotify）的兜底，也是测试的确定性入口（测试发 SIGHUP 即可断言重载，不依赖文件系统事件时序）。

- 备选：轮询 mtime —— 简单但有延迟/CPU 取舍，且无法区分原子 rename，弃用。
- 备选：仅 SIGHUP —— 满足"手改后生效"需用户主动发信号，体验差；保留为兜底而非唯一手段。
- 新增依赖 `github.com/fsnotify/fsnotify`（跨平台，成熟）。WSL 下 `~/.workhorse-agent` 位于 Linux 家目录，inotify 正常（仅 `/mnt/*` 跨盘路径不可靠，不涉及）。

### D2：重载范围 — 只应用"权限子集"，其余 diff 后 WARN
重载回调重新 `config.Load(YAMLPath, LookupEnv)` 得到 `newCfg`，与"当前生效配置快照"逐字段比较：
- 应用：`tools.preset_rules`（重跑 `applyPresetRules`）、`tools.default_permission`（Manager setter）、`agent.permission_request_timeout_seconds`（Manager setter）。
- 检测到但不应用：`store.path`、`server.host/port`、`providers.*` 等任一变化 → `logger.Warn("field changed; restart required", "field", ...)`。

这样把热加载的爆炸半径限制在权限三字段，彻底回避重绑 HTTP 监听、重开 DB 连接、重建 provider 这类高危操作。

### D3：生效路径 — 复用 Check 的实时 store 读 + 加锁 setter
preset 规则无需任何新机制：`applyPresetRules` 写完 SQLite，`Check()` 下次实时读到。`default_permission` 与 timeout 当前是 Manager 的不可变字段，新增 `SetDefaultDecision(Decision)` 与 `SetTimeout(time.Duration)`，用 Manager 已有的 `mu` 保护；`Check()` 读这两个值时同样取锁（或改为 atomic.Value）。对账 `applyPresetRules` 的多行写入建议包一个事务，避免 `Check()` 读到半更新规则集（即便不包事务，单行原子 + deny>allow 逐查询计算，瞬时不一致也可接受）。

### D4：配置读写端点 — yaml.v3 Node 外科式编辑
`GET /v1/permission-config`：读 `config.yaml`，反序列化后只取 `tools.preset_rules` 与 `tools.default_permission` 返回；文件不存在则返回内置默认。

`PUT /v1/permission-config`：用 `yaml.v3` 的 `yaml.Node`（document/mapping node）解析整个文件树，**只定位并替换 `tools` 映射下的 `preset_rules` 与 `default_permission` 两个子节点**（不存在则插入），再 `Encode` 回写。`yaml.Node` 保留 `HeadComment/LineComment/FootComment` 与键顺序，从而保住用户注释与文件其余内容。写入走"临时文件 + `os.Rename`"原子替换，避免 reader 读到截断文件，也自然触发 D1 的目录监听完成闭环。

- 备选：`serde`/marshal 整个 struct 回写 —— 会丢失全部注释，弃用。
- 备选：assistant 侧 Rust 直接写文件 —— Rust `serde_yaml` 不保留注释，且远程模式写不到对方文件；故由 agent 统一负责（API 远程可达 + 注释逻辑只在 Go 写一遍）。
- 校验：handler 先对 body 的 `decision`/`default_permission` 做白名单校验（400 含合法取值），写入后由热加载的 `config.Load()` 做完整校验兜底。

### D5：单一真源
PUT 只改 `config.yaml`，绝不写 SQLite 或调 `/v1/permissions`。`GET /v1/permissions` 仍可用于"回读当前真正在 store 中生效的全部规则"（含 preset 与 perm 来源），与本端点（读文件真源）职责区分清楚。

## Risks / Trade-offs

- **[半成品文件触发重载]** → 目录监听 + debounce + `config.Load()` 校验闸门三重防护；校验失败保留旧配置。
- **[PUT 写入与外部手改/监听竞态]** → PUT 用原子 rename；其自身触发的监听事件重载结果与 PUT 写入内容一致（幂等），重复对账无副作用。
- **[Manager setter 与 Check 并发]** → 复用既有 `mu` 或改 `atomic.Value`；写少读多，开销可忽略。
- **[fsnotify 在个别 FS/容器不可用]** → SIGHUP 兜底；并在启动日志中标明监听是否就绪。
- **[远程模式文件路径]** → 端点读写的是 **agent 进程所在主机** 的 `~/.workhorse-agent/config.yaml`，语义正确；assistant 远程模式通过 HTTP 调用即作用于远端真源。
- **[watcher goroutine 泄漏]** → 与 `serve` 的 ctx 绑定，优雅关停路径中 `watcher.Close()` 并退出 goroutine。

## Migration Plan

向后兼容，无数据迁移：
1. 新增 `fsnotify` 依赖与 watcher 接线；不改变启动时的一次性加载行为（热加载是叠加能力）。
2. 新增 Manager setter，默认行为不变。
3. 新增两个 HTTP 端点；旧客户端不受影响。
4. 回滚：移除 watcher 接线与端点即可回到"启动加载一次"的旧行为，配置文件格式不变。

## Open Questions

- debounce 窗口取值（200ms 起步，可按实测调整）。
- 端点路径最终命名：`/v1/permission-config` vs 归入 `/v1/config/permissions`（暂定前者，specs 已采用）。
- 是否需要在 `GET /v1/permission-config` 响应中附带"是否已被热加载生效"的指示（当前由 assistant 用 `GET /v1/permissions` 回读判断，暂不在本端点内联）。
