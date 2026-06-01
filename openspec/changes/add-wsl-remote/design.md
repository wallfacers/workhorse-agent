# Design: WSL 远程 sidecar 端点

> 本文档把 **交付顺序** 定死:batch 2 的三个端点是 WSL 远程的硬门控。
> 依赖图与跨 change 账本让"先做什么、后做什么、为什么"一眼可见。
> batch 1 的五个端点设计在 `add-project-sessions/design.md`,本文档只引用。

## 依赖图:端点 → 功能解锁

```
SIDECAR ENDPOINT (Go)                          UNLOCKS (assistant — code already in place)
───────────────────────────────────────────    ────────────────────────────────────────────────
BATCH 1 (add-project-sessions, 无需 WSL)
  GET  /v1/sessions?workdir=           ──────▶ 切换器显示项目会话列表
  GET  /v1/sessions/{id}/history       ──────▶ 切回旧会话 UI 重建 + §3.5 淘汰恢复
  GET  /v1/projects                    ──────▶ 项目下拉框
  PATCH/DELETE /v1/sessions/{id}       ──────▶ ⋯ 菜单重命名/删除
  tool_call_done {output,error}        ──────▶ 工具结果可见

BATCH 2 (本变更, WSL 激活)
  GET  /health { default_workdir }     ──────▶ §1.7 冷启动无 host-cwd 回退
                                           ───▶ 零摩擦首次启动
  /health capabilities {platform,      ──────▶ UI 默认终端为 wsl profile
                distro}                ────▶ 知道在与远程 sidecar 通信
  GET  /v1/fs/list?path=               ──────▶ 命名空间正确的目录浏览
```

```
DELIVERY ORDER
  batch 1 ─ sessions / history / projects / rename / delete / tool output
            → finishes add-project-sessions end-to-end (no WSL involved)
                 │
                 ▼
  batch 2 ─ /health default_workdir + capabilities + fs/list   ← 本变更
            → enables WSL remote
                 │
                 ▼
  assistant-side WSL work (§1.7 flip, terminal profile, decoupling)
            → in workhorse-assistant repo, tasks B1-B4 / C1-C3
```

**关键结论:不启动 assistant 侧 WSL 工作直到 batch 2 端点落地;
不润色切换器/淘汰直到 batch 1 端点落地。**

## 决策

### D-WSL-1 — §1.7 lives here, not in add-project-sessions

去掉 Rust host-cwd 回退要求所有时候显式传 `workdir`;保持零配置启动又要求
`/health default_workdir`,后者只在 WSL capabilities 变更中落地。提前翻转 §1.7
会破坏"打开 app → 直接聊天"。

### D-WSL-2 — 冷启动:sidecar 自报默认,不是 host 检测

assistant 永远不猜路径。优先链:记住的上次项目 → `/health default_workdir` →
项目选择器。renderer 保持命名空间无感知。

`default_workdir` 取值策略:
1. 配置文件 `server.default_workdir`(显式设置,最高优先)
2. `os.Getwd()`(sidecar 进程的工作目录,默认值)

这个值在 `/health` 响应时动态计算(不用持久化)。

### D-WSL-3 — 项目路径是 sidecar 命名空间的不透明字符串

sidecar 不做 `\\wsl$\` ↔ `/…` 翻译。如果需要翻译,只发生在 assistant 侧。
优先使用 sidecar 提供的枚举(`fs/list`),让选择器从构造上命名空间正确。

### D-WSL-4 — Full health/session decoupling 随此变更一起做

§3.6 后续(`useAgentConnection` → 纯健康探针;per-session `connection_failed`
重开而非新建会话)只在远程 sidecar 导致连接掉线常见化、启动改为选择器驱动之后
才值得做。在那之前,`add-project-sessions` 中已交付的 store 层 bootstrap-replacement
修复(commit `127dbfd`)保持 B3 可控。

### D-WSL-5 — 终端是 80% 方案

`wsl.exe -d <distro> --cd <path>`;PTY 留在 Windows host,shell 进程是 WSL 桥。
不做 server-side PTY(超出范围)。

### D-WSL-6 — `/v1/fs/list` 走 pathguard + 虚拟 FS 黑名单

路径经 `internal/tools/pathguard` 的完整管道(`Clean` → `EvalSymlinks` →
`Rel` 检查)。额外维护一个虚拟文件系统前缀黑名单(`/proc`, `/sys`, `/dev`,
`/run`)以避免枚举无意义或可能有副作用的条目。首版单层枚举(不递归),无分页。

## 跨 change 账本(谁移到了哪)

| Item | From | To | Status |
| --- | --- | --- | --- |
| §1.7 explicit workdir | add-project-sessions §3.6 | → assistant add-wsl-remote (D-WSL-1) | moved |
| `useAgentConnection` decoupling | add-project-sessions §3.6 | → assistant add-wsl-remote (D-WSL-4) | moved |
| §3.5 memory eviction | add-project-sessions | stays; **门控于 `GET /history`** | stays |
| Native folder picker | add-project-sessions §4.5 | → **本变更** (`fs/list`, D-WSL-3) | → Go sidecar |
| Settings editable endpoint | add-project-sessions §4.6 | → assistant add-wsl-remote | moved |
| `tool_call_done` output | add-project-sessions C1/T6 | batch 1 (add-project-sessions tasks §4.3) | stays |
| T7 history 完备性 | add-project-sessions workhorse-agent-tasks §T7 | → **本变更** workhorse-agent-tasks | → here |

## `GET /v1/fs/list` 端点设计

### 请求

```
GET /v1/fs/list?path=/home/user
GET /v1/fs/list                  (省略 path → 用 default_workdir)
```

### 响应

```json
{
  "path": "/home/user",
  "entries": [
    { "name": "Documents", "path": "/home/user/Documents", "isDir": true },
    { "name": "project",   "path": "/home/user/project",   "isDir": true },
    { "name": ".bashrc",   "path": "/home/user/.bashrc",   "isDir": false }
  ]
}
```

### 错误

| 条件 | HTTP 状态 |
| --- | --- |
| 路径不存在 | `404 Not Found` |
| 路径不是目录 | `400 Bad Request` |
| 路径在黑名单内 | `403 Forbidden` |
| 路径逃逸 / 无权限 | `403 Forbidden` |

### 实现路径

1. 新文件 `internal/api/fs.go`:
   - `handleFSList(w, r)` handler
   - 调用 `pathguard.Resolve(r)` 做路径校验
   - 检查虚拟 FS 黑名单(`isVirtualFS(path)`)
   - `os.ReadDir(path)` 枚举
   - 返回 `FSEntry{Name, Path, IsDir}` 数组

2. `internal/api/server.go`:
   - 注册 `mux.HandleFunc("GET /v1/fs/list", s.handleFSList)`

### WSL 检测

`isWSL()` 函数读 `/proc/version`,匹配 `(microsoft|WSL)`。编译进 `linux`
构建标签,非 Linux 平台返回 `false`。

`distro` 取值:解析 `/etc/os-release` 的 `PRETTY_NAME` 或 `NAME` 行。
WSL 专用备选:解析 `/proc/version` 中的发行版字符串。

## `/health` 增强设计

### 响应形状(新增字段标 `// NEW`)

```json
{
  "ok": true,
  "version": "0.4.0",
  "protocol_version": "1",
  "uptime_sec": 3600,
  "sessions_active": 2,
  "capabilities": ["frontend_tools", "external_agents"],
  "default_workdir": "/home/user",        // NEW: string
  "platform": "linux",                     // NEW: runtime.GOOS
  "distro": "Ubuntu-22.04"                 // NEW: 仅 WSL 时出现
}
```

`distro` 仅在 `isWSL() == true` 时包含,避免非 WSL Linux 乱报。
`default_workdir` 始终出现(不可能缺失,最少是 `os.Getwd()`)。

### 实现路径

修改 `internal/api/health.go`:
- 新增 `defaultWorkdir` 计算(配置优先 → `os.Getwd()` 兜底)
- 新增 `platform` = `runtime.GOOS`
- 条件性新增 `distro`(WSL 检测)
- 不改现有的 `ok`/`version`/`protocol_version`/`uptime_sec`/`sessions_active`/`capabilities` 字段

## 风险 / 待验证

- **pathguard 与 `fs/list` 的交互**:现有 pathguard 设计为保护 session workdir
  内的文件操作;`fs/list` 的语义是浏览任意可读目录。需要一个独立的校验函数,只做
  `Clean` + `EvalSymlinks` + 虚拟 FS 黑名单,不做 session workdir `Rel` 检查。
- **符号链接跟随**:目录枚举中的 symlink 目标可能逃逸。`EvalSymlinks` 解析后
  再做黑名单检查即可。不递归因此不存在 symlink 炸弹风险。
- **大目录性能**:`os.ReadDir` 返回全部条目;极大量目录(>10k)可能慢。
  首版不设上限,后续可加 `?limit=` 参数。
- **WSL 检测稳定性**:依赖 `/proc/version` 字符串匹配,WSL 版本升级可能变化。
  匹配模式应同时包含 `microsoft` 和 `WSL`(大小写不敏感)。

## 开放问题(实现时定夺)

1. `fs/list` 是否需要隐藏文件过滤(默认显示 dotfiles 还是隐藏)?
2. `fs/list` 是否需要按名称排序(默认 `os.ReadDir` 已排序)?
3. `default_workdir` 是否需要在配置变更后立即生效(当前设计每次请求时计算,已满足)?
