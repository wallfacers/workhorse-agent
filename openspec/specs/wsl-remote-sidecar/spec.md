## ADDED Requirements

### Requirement: Health 端点报告默认工作目录与平台能力

`GET /health` 响应 SHALL 包含 `default_workdir`(`string`,sidecar 的默认项目路径)
与 `platform`(`string`,`runtime.GOOS`,如 `linux`/`windows`/`darwin`)。
当 sidecar 检测到运行在 WSL2 内时,响应 SHALL 额外包含 `distro`(`string`,
Linux 发行版标识,如 `Ubuntu-22.04`);非 WSL 环境 SHALL NOT 包含 `distro`。

`default_workdir` 取值 SHALL 优先使用配置项 `server.default_workdir`(若非空),
否则 SHALL 使用 `os.Getwd()`(sidecar 进程工作目录)。该值 SHALL 始终非空。

所有新增字段 SHALL 向后兼容——既有的 `/health` 消费者 SHALL NOT 受到影响。

#### Scenario: 非 WSL Linux 的 health 响应

- **WHEN** sidecar 运行在非 WSL 的 Linux 上
- **THEN** `GET /health` 返回 `default_workdir`(非空)、`platform: "linux"`,
  不包含 `distro` 字段

#### Scenario: WSL2 的 health 响应

- **WHEN** sidecar 运行在 WSL2 内(`Ubuntu-22.04`)
- **THEN** `GET /health` 返回 `default_workdir`(非空)、`platform: "linux"`,
  `distro: "Ubuntu-22.04"`

#### Scenario: 配置覆盖 default_workdir

- **WHEN** 配置文件设置 `server.default_workdir: "/opt/projects"`
- **THEN** `GET /health` 的 `default_workdir` 为 `/opt/projects`(而非进程 cwd)

### Requirement: 文件系统目录枚举端点

服务 SHALL 暴露 `GET /v1/fs/list?path=<dir>` 端点,枚举 sidecar 文件系统中
指定目录的条目。每个条目 SHALL 包含 `name`(文件/目录名)、`path`(完整路径)
和 `isDir`(布尔)。`path` 参数省略时 SHALL 使用 `default_workdir` 作为枚举根。

路径 SHALL 经 `filepath.Clean` + `filepath.EvalSymlinks` 处理。服务 SHALL
拒绝虚拟文件系统前缀(`/proc`、`/sys`、`/dev`、`/run`),返回 `403 Forbidden`。
路径不存在时 SHALL 返回 `404 Not Found`;非目录路径 SHALL 返回 `400 Bad Request`。

枚举 SHALL 为单层(不递归),包含 dotfiles,按文件名排序。服务 SHALL NOT
递归进入子目录。

#### Scenario: 枚举存在的目录

- **WHEN** 客户端 `GET /v1/fs/list?path=/home/user`
- **THEN** 返回 `200 OK` 和 `{ "path": "/home/user", "entries": [{ "name": "Documents", "path": "/home/user/Documents", "isDir": true }, ...] }`

#### Scenario: 省略 path 使用默认路径

- **WHEN** 客户端 `GET /v1/fs/list`(无 query 参数)
- **THEN** 返回 `200 OK`,以 `default_workdir` 为枚举根

#### Scenario: 虚拟文件系统路径被拒绝

- **WHEN** 客户端 `GET /v1/fs/list?path=/proc`
- **THEN** 返回 `403 Forbidden`

#### Scenario: 不存在的路径

- **WHEN** 客户端 `GET /v1/fs/list?path=/nonexistent`
- **THEN** 返回 `404 Not Found`

#### Scenario: 文件路径而非目录

- **WHEN** 客户端 `GET /v1/fs/list?path=/etc/passwd`
- **THEN** 返回 `400 Bad Request`

### Requirement: History 完备性(支撑前端内存淘汰)

`GET /v1/sessions/{id}/history` 的 `parts[]` SHALL 覆盖该会话所有**已完成轮次**
中的 `text` 与 `tool_call` 内容块。`tool_call` part SHALL 包含 `id`、`name`、
`input`、`status`(`"done"` 或 `"error"`)与 `output`(若有)。`tool_result`
SHALL 按 `toolUseId` 回填到对应 `tool_call` part 的 `output`/`status`,不单独
成 part。

此完备性是 assistant 侧 idle 会话内存淘汰的硬前置:淘汰后切回旧会话,
`GET …/history` 重建的 transcript SHALL 与淘汰前 UI 所见一致。
正在流式的、未结束的轮次内容不在此范围内(assistant 只淘汰 idle 会话)。

#### Scenario: 工具调用的输出在 history 中可见

- **WHEN** 一个已完成会话中有一轮含工具调用(Bash `ls -la`)及返回结果
- **THEN** `GET …/history` 的 `tool_call` part 包含 `output`(工具输出)
  和 `status: "done"`

#### Scenario: 工具调用失败在 history 中标为 error

- **WHEN** 一个已完成会话中有一轮工具调用返回错误
- **THEN** `GET …/history` 的 `tool_call` part 包含 `error` 信息
  和 `status: "error"`
