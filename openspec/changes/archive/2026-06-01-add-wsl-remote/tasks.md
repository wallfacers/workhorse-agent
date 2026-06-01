# Tasks: WSL 远程 sidecar 端点

> **前置**:batch 1(add-project-sessions 的全部 pending 任务:
> Groups 2-8:水合、status 投影、5 个 HTTP 端点、title 派生、验收)必须先完成。
> 本变更只包含 batch 2 的三个 sidecar 端点增量。
> 交付顺序见 [`design.md`](./design.md)。

## A. `/health` 增强

- [x] A1 在 `internal/api/health.go` 的响应中新增 `default_workdir` 字段(`string`):
  取值优先链:`config.Server.DefaultWorkdir`(非空)→ `os.Getwd()`。始终出现。
- [x] A2 在 `/health` 响应中新增 `platform` 字段(`string` = `runtime.GOOS`)。
- [x] A3 在 `/health` 响应中条件性新增 `distro` 字段:仅在 `isWSL()` 返回 `true`
  时出现,值取 `/etc/os-release` 的 `PRETTY_NAME`(或 `NAME`)。
- [x] A4 实现 `isWSL()` 函数:读 `/proc/version`,匹配 `(?i)(microsoft|wsl)`。
  编译进 `linux` 构建标签,非 Linux 返回 `false`。
- [x] A5 (可选)在 `internal/config/config.go` 新增 `Server.DefaultWorkdir string`
  配置项;配置缺省时 `default_workdir` 纯靠 `os.Getwd()`。
- [x] A6 补测试:断言 `/health` 响应包含 `default_workdir`/`platform`;断言非 WSL
  环境无 `distro` 字段。

## B. `GET /v1/fs/list` 端点

- [x] B1 新建 `internal/api/fs.go`:
  - `FSEntry` 结构:`{ Name string, Path string, IsDir bool }`
  - `FSListResponse` 结构:`{ Path string, Entries []FSEntry }`
  - `handleFSList(w http.ResponseWriter, r *http.Request)` handler
- [x] B2 路径校验:
  - query 参数 `path` 省略 → 用 `default_workdir`(同 A1 逻辑)
  - `filepath.Clean` + `filepath.EvalSymlinks`
  - 虚拟 FS 黑名单检查(`isVirtualFS`):前缀匹配 `/proc`/`/sys`/`/dev`/`/run`
  - `os.Stat` 确认目标是目录(非目录 → `400`,不存在 → `404`)
- [x] B3 目录枚举:`os.ReadDir(path)`,逐条映射为 `FSEntry`(拼接完整 `path`);
  `ReadDir` 本身已按文件名排序。包含 dotfiles。
- [x] B4 错误响应:
  - 路径不存在 → `404 Not Found` + `{"error":"not found"}`
  - 非目录 → `400 Bad Request` + `{"error":"not a directory"}`
  - 虚拟 FS / 无权限 → `403 Forbidden` + `{"error":"forbidden"}`
- [x] B5 在 `internal/api/server.go` 注册路由:
  `mux.HandleFunc("GET /v1/fs/list", s.handleFSList)`
- [x] B6 补测试:
  - 正常目录返回 entries(含 name/path/isDir)
  - 省略 `path` 用 default_workdir
  - 不存在的路径 → 404
  - 文件路径 → 400
  - `/proc` → 403
  - symlink 目标正确解析

## C. T7 history 完备性契约(跨 change 前置)

> 这不是一个新端点,而是对 `add-project-sessions` 中 `GET …/history` 端点的
> 质量约束。放在本变更的 `workhorse-agent-tasks.md` 中固化,因为它是 assistant
> 侧 idle 会话内存淘汰(§3.5)的**硬前置**。

- [x] C1 确认 `GET …/history` 的 `parts[]` 覆盖所有已完成轮次的:
  - `text` part(所有角色消息的文本内容)
  - `tool_call` part(含 `id`/`name`/`input`/`output`/`status`)
  - tool_result 按 `toolUseId` 回填到对应 tool_call 的 `output`/`status`
- [x] C2 补集成测试:创建一个会话,发一轮含工具调用的完整对话,调用 history,
  断言 parts 包含 text + tool_call(含 output + status)。
- [x] C3 验证:assistant 的 idle 会话淘汰后,切回 → `GET …/history` 重建的
  transcript 与淘汰前 UI 所见一致。

## D. 端到端验收(WSL 远程)

- [x] D1 `GET /health` 返回 `default_workdir`(非空)、`platform`(`linux`);
  WSL 环境下额外有 `distro`。
- [x] D2 `GET /v1/fs/list?path=/home` 返回该目录下的条目列表(含 name/path/isDir)。
- [x] D3 `GET /v1/fs/list`(省略 path)使用 default_workdir 作为枚举根。
- [x] D4 `GET /v1/fs/list?path=/proc` → 403。
- [x] D5 `golangci-lint run` 干净;gofumpt 通过。

## 回填(实现后)

- 若实际字段名/路由/行为与本文不同,列差异,知会 assistant 同步
  `src-tauri/src/agent/mod.rs` 顶部常量。
