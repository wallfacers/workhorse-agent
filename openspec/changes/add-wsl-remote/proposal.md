# Proposal: WSL 远程 sidecar 端点(冷启动 + 平台感知 + 目录浏览)

> **Status: DETAILED.** 本变更只涉及 Go sidecar 的三个端点增量,不涉及
> assistant (Tauri/TS) 侧代码。assistant 侧的 WSL 工作(§1.7 翻转、连接解耦、
> 终端 profile、项目浏览器)在 assistant 仓的 `add-wsl-remote` change 中单独管理。
> 依赖图与交付顺序见 [`design.md`](./design.md),sidecar 任务清单见
> [`tasks.md`](./tasks.md),完整交付契约(含 T7)见
> [`workhorse-agent-tasks.md`](./workhorse-agent-tasks.md)。

## Why

`workhorse-assistant`(Tauri 桌面端)面向 WSL 远程场景:UI 在 Windows 原生运行,
`workhorse-agent` sidecar 跑在 WSL2 内,项目路径是 sidecar 命名空间下的
`/home/user/proj`。assistant 侧的 `add-project-sessions` 已闭合——桥代码全部就绪,
只等 batch 1 的五个端点落地。

WSL 远程场景额外需要三个 sidecar 能力,当前全部缺失:

1. **零配置冷启动需要 sidecar 自报默认路径**。assistant 的 §1.7 决策(D-WSL-1)
   要求去掉 Rust 侧 `std::env::current_dir()` 的 host-cwd 回退,改为始终显式传
   `workdir`。保持"打开 app → 直接聊天"的零摩擦体验则要求 sidecar 在 `/health`
   报告 `default_workdir`,assistant 用它作为首次启动的初始项目路径。
   没有 `default_workdir`,§1.7 翻转会强制新用户先过项目选择器。

2. **UI 需要知道 sidecar 的平台与能力**。assistant 要根据 sidecar 平台默认终端
   profile(Windows sidecar → PowerShell;WSL sidecar → `wsl.exe`)、决定是否展示
   命名空间相关的 UI 提示。当前 `/health` 的 `capabilities` 只有
   `["frontend_tools", "external_agents"]`,缺少平台/发行版信息。

3. **项目浏览器需要在 sidecar 命名空间枚举目录**。Windows 原生文件夹对话框看到的
   是 Windows 路径,不是 WSL 路径。需要 sidecar 提供一个目录枚举端点,让浏览器在
   sidecar 的文件系统内工作,命名空间正确由构造保证(D-WSL-3)。

## What Changes

- **`/health` 增强**:在既有响应中新增 `default_workdir`(`string`,sidecar 的
  默认项目路径,取 `os.Getwd()`,可由 `server.default_workdir` 配置覆盖)与
  `capabilities` 数组中的 `platform`(e.g. `linux`/`windows`)字段;当 sidecar
  检测到运行在 WSL 内时额外暴露 `distro`(e.g. `Ubuntu-22.04`)。全部向后兼容——
  assistant 容忍新增字段。

- **`GET /v1/fs/list?path=<dir>`(新增端点)**:枚举 sidecar 文件系统中指定目录
  的条目,返回 `{ "entries": [{ "name": "...", "path": "...", "isDir": true }] }`。
  `path` 省略时用 sidecar 默认路径。路径经 `pathguard` 校验(不允许逃逸到不可读
  区域);拒绝虚拟文件系统(`/proc`, `/sys`, `/dev`)。无认证要求(与现有端点一致,
  均为 loopback-only)。

- **T7 history 完备性契约**:明确 history 端点(`GET /v1/sessions/{id}/history`)
  的 `parts[]` 必须覆盖已完成轮次中的 `text` + `tool_call`(含 `output`/`status`)
  以支撑 assistant 侧的 idle 会话内存淘汰。这是 batch 1 的延续约束,不是新端点,
  但需要在 `workhorse-agent-tasks.md` 中作为硬前置固化。

## Capabilities

### New Capabilities

- `wsl-remote-sidecar`:sidecar 报告平台/默认路径并提供命名空间内目录枚举,
  使 assistant 能在 WSL 远程模式下零摩擦冷启动、正确默认终端、浏览 sidecar 文件系统。

### Modified Capabilities

- `api-protocol`:`/health` 响应新增 `default_workdir`;`capabilities` 新增
  `platform`/`distro`;新增 `GET /v1/fs/list` 端点。

## Impact

- **Code**:
  - `internal/api/health.go` — 新增 `default_workdir`、`platform`/`distro` 字段;
    WSL 检测逻辑(`/proc/version` 含 `microsoft` 或 `WSL`)。
  - `internal/api/fs.go`(新文件) — `GET /v1/fs/list` handler + `pathguard` 集成。
  - `internal/api/server.go` — 注册 `/v1/fs/list` 路由。
  - `internal/config/config.go`(可选) — `server.default_workdir` 配置项。
- **Cross-repo**:闭合 assistant `add-wsl-remote` 的 A1/A2/A3 任务依赖。
- **Backward compatibility**:所有新增字段/端点向后兼容;既有的 `/health` 消费者
  不受影响。
- **Out of scope(本轮不做)**:server-side PTY;`\\wsl$\` 路径翻译;SSH/容器远程;
  多 distro 并发;目录递归深度/分页(首版单层)。
