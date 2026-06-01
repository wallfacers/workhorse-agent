# workhorse-agent 任务书 — WSL 远程 sidecar 端点(可直接交付 Go 仓)

> **给在 `workhorse-agent`(Go)仓工作的实现者(人或 AI)的自包含任务书。**
> 你**不需要**读 assistant(Tauri/TS)仓的任何代码。本文档把对接所需的全部约定
> 写全了:端点、HTTP 方法、请求/响应形状(**字段名一律 camelCase**)。
>
> **前置**:本变更依赖 `add-project-sessions` 的全部 pending 任务(Groups 2-8)
> 先完成。本任务书只包含 batch 2 的三个 sidecar 端点增量 + T7 history 完备性。
>
> 字段名、路径、方法是**契约**:assistant 已按这些发请求/解析响应,Go 侧需对齐。

---

## 1. 这个功能是什么

WSL 远程模式下,assistant 的 UI 跑在 Windows,sidecar 跑在 WSL2 内。assistant 需要:

1. **零配置冷启动**:sidecar 在 `/health` 自报 `default_workdir`,assistant 用它
   作为首次启动的初始项目路径(无需 host-cwd 回退)。
2. **平台感知**:sidecar 在 `/health` 暴露 `platform`/`distro`,assistant 据此
   默认终端 profile(WSL → `wsl.exe`)。
3. **命名空间内目录浏览**:sidecar 提供 `GET /v1/fs/list`,让项目选择器在
   sidecar 文件系统内工作(Windows 文件夹对话框看到的是错误命名空间)。
4. **history 完备性**(T7):`GET …/history` 的 `parts[]` 必须能无损重建 UI 所见,
   是 assistant 侧 idle 会话内存淘汰的硬前置。

---

## 2. 端点契约

### 2.1 `GET /health` 增强(不新增端点,扩展现有响应)

**现有响应**(不变):

```json
{
  "ok": true,
  "version": "0.4.0",
  "protocol_version": "1",
  "uptime_sec": 3600,
  "sessions_active": 2,
  "capabilities": ["frontend_tools", "external_agents"]
}
```

**新增字段**(追加到同一响应):

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `default_workdir` | `string` | ✅ 始终 | sidecar 默认项目路径,非空 |
| `platform` | `string` | ✅ 始终 | `runtime.GOOS`(`linux`/`windows`/`darwin`) |
| `distro` | `string` | ❌ 条件 | 仅 WSL 时出现;Linux 发行版标识 |

assistant 解析约定:
- 读 `default_workdir` 作为冷启动初始项目路径
- 读 `platform` 决定终端默认 profile
- 读 `distro` 决定 WSL distro(如 `Ubuntu-22.04`);缺失则非 WSL

`default_workdir` 取值:
1. 配置 `server.default_workdir`(若非空)→ 优先
2. `os.Getwd()`(sidecar 进程 cwd)→ 兜底

WSL 检测:读 `/proc/version`,匹配 `(?i)(microsoft|wsl)`。编译进 `linux`
构建标签。

`distro` 取值:解析 `/etc/os-release` 的 `PRETTY_NAME`(或 `NAME`)行。

### 2.2 `GET /v1/fs/list?path=<dir>` (新增端点)

**请求**:

```
GET /v1/fs/list?path=/home/user      (指定路径)
GET /v1/fs/list                       (省略 path → 用 default_workdir)
```

**成功响应** (`200 OK`):

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

**错误响应**:

| 条件 | HTTP 状态 | body |
|---|---|---|
| 路径不存在 | `404` | `{ "error": "not found" }` |
| 非目录 | `400` | `{ "error": "not a directory" }` |
| 虚拟 FS / 无权限 | `403` | `{ "error": "forbidden" }` |

assistant 解析约定:取 `.entries` 数组;每条 `isDir === true` 渲染为可展开文件夹,
`isDir === false` 渲染为文件(或忽略)。

虚拟文件系统黑名单(前缀匹配):
- `/proc`
- `/sys`
- `/dev`
- `/run`

枚举规则:
- 单层,不递归
- 包含 dotfiles
- `os.ReadDir` 已按文件名排序

### 2.3 T7 history 完备性(对 `add-project-sessions` 的 `GET …/history` 约束)

**不是新端点**。是对 `add-project-sessions` 中 `GET /v1/sessions/{id}/history`
端点的质量约束。

`parts[]` 必须覆盖所有**已完成轮次**的:

| part 类型 | 必需字段 | 说明 |
|---|---|---|
| `text` | `type`, `content` | 所有角色消息的文本内容 |
| `tool_call` | `type`, `id`, `name`, `input`, `status`, `output?` | 含工具输出与状态 |
| `reasoning` | `type`, `text`, `redacted?` | 可选(缺失只少思考块) |

`tool_result` 按 `toolUseId` 回填到对应 `tool_call` 的 `output`/`status`,
不单独成 part。

`status` 枚举:`"done"`(成功)| `"error"`(失败)。

assistant 淘汰边界:只淘汰 `idle` 会话。`running` 会话的 in-flight delta 还没落盘,
不在 transcript 里——所以 history 只需覆盖**已完成的轮次**。

---

## 3. 行为要求(任务项)

- **A1 `default_workdir`**:`/health` 响应始终含 `default_workdir`(非空)。
  取值优先链:配置 → `os.Getwd()`。
- **A2 `platform`**:`/health` 响应始终含 `platform`(`runtime.GOOS`)。
- **A3 `distro`**:`/health` 响应仅在 WSL 时含 `distro`(Linux 发行版标识)。
- **B1 `fs/list`**:实现 `GET /v1/fs/list?path=` 端点,按 §2.2 契约。
- **B2 路径安全**:所有路径经 `filepath.Clean` + `filepath.EvalSymlinks`;
  虚拟 FS 前缀黑名单; symlink 解析后做检查。
- **T7 history 完备性**:确认 `GET …/history` 的 `parts[]` 覆盖已完成轮次中
  `text` + `tool_call`(含 `output`/`status`);补集成测试。

---

## 4. 实现指引

### 文件清单

| 文件 | 变更 |
|---|---|
| `internal/api/health.go` | 新增 `default_workdir`/`platform`/`distro` 字段 + `isWSL()` |
| `internal/api/fs.go` | **新建**: `handleFSList` handler + `FSEntry`/`FSListResponse` |
| `internal/api/server.go` | 注册 `GET /v1/fs/list` 路由 |
| `internal/config/config.go` | (可选) `Server.DefaultWorkdir string` |
| `internal/api/health_test.go` | 补断言 |
| `internal/api/fs_test.go` | **新建**:完整测试 |

### WSL 检测

```go
//go:build linux

func isWSL() bool {
    b, err := os.ReadFile("/proc/version")
    if err != nil { return false }
    s := strings.ToLower(string(b))
    return strings.Contains(s, "microsoft") || strings.Contains(s, "wsl")
}
```

非 Linux 平台 `isWSL()` 返回 `false`(stub)。

### pathguard 复用注意

现有 `pathguard` 包(`internal/tools/pathguard`)设计为保护 session workdir
内的文件操作(含 `Rel` 检查)。`fs/list` 的语义是浏览任意可读目录,不应受
session workdir 约束。建议:
- 复用 `filepath.Clean` + `filepath.EvalSymlinks` 逻辑
- 不做 `Rel` 检查(无 session workdir 上下文)
- 单独实现虚拟 FS 黑名单检查

---

## 5. 边界 / 前向兼容(本轮不做)

- 目录递归 / 深度限制(首版单层)
- 分页 / `?limit=` 参数
- 文件内容预览
- `\\wsl$\` 路径翻译(sidecar 不做,如果需要只在 assistant 侧)
- 多 distro 并发
- SSH / 容器远程(非 WSL)

---

## 6. 验收(对齐 assistant 端到端)

1. `GET /health` 返回 `default_workdir`(非空)和 `platform`(`linux`)。
2. WSL 环境:额外返回 `distro`(如 `Ubuntu-22.04`)。
3. `GET /v1/fs/list?path=/home` 返回条目列表(含 name/path/isDir)。
4. `GET /v1/fs/list`(省略 path)使用 default_workdir。
5. `GET /v1/fs/list?path=/proc` → 403。
6. `GET /v1/fs/list?path=/nonexistent` → 404。
7. history 完备性:一轮含工具调用的完整对话后,`GET …/history` 的 tool_call part
   包含 `output` + `status`。
8. `golangci-lint run` 干净;gofumpt 通过。

---

## 7. 回填(实现后)

- 若实际路由/字段与本文不同,列差异,assistant 侧会同步
  `src-tauri/src/agent/mod.rs` 顶部常量。
