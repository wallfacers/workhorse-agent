## MODIFIED Requirements

### Requirement: Health 端点报告默认工作目录与平台能力

`GET /health` 响应 SHALL 包含 `platform`(`string`,`runtime.GOOS`,如
`linux`/`windows`/`darwin`)。当 sidecar 检测到运行在 WSL2 内时,响应 SHALL 额外
包含 `distro`(`string`,Linux 发行版标识,如 `Ubuntu-22.04`);非 WSL 环境
SHALL NOT 包含 `distro`。

`default_workdir`(`string`,sidecar 的默认项目路径)取值 SHALL 优先使用配置项
`server.default_workdir`(若非空),否则 SHALL 使用 `os.UserHomeDir()`(用户主
目录)。它 SHALL NOT 回退到 `os.Getwd()`(sidecar 进程启动目录)——启动目录是
长驻进程如何被拉起的偶然结果,绝非有意义的项目。当既无配置覆盖、又无法解析主目录
时,响应 SHALL 省略 `default_workdir`(或返回空串),以便客户端转入项目选择器,
而非报告启动目录。

所有新增字段 SHALL 向后兼容——既有的 `/health` 消费者 SHALL NOT 受到影响。

#### Scenario: 非 WSL Linux 的 health 响应

- **WHEN** sidecar 运行在非 WSL 的 Linux 上,且未配置 `server.default_workdir`
- **THEN** `GET /health` 返回 `default_workdir` 等于用户主目录、`platform: "linux"`,
  不包含 `distro` 字段
- **AND** 该值不是 sidecar 进程的启动目录

#### Scenario: WSL2 的 health 响应

- **WHEN** sidecar 运行在 WSL2 内(`Ubuntu-22.04`),且未配置 `server.default_workdir`
- **THEN** `GET /health` 返回 `default_workdir` 等于用户主目录、`platform: "linux"`,
  `distro: "Ubuntu-22.04"`

#### Scenario: 配置覆盖 default_workdir

- **WHEN** 配置文件设置 `server.default_workdir: "/opt/projects"`
- **THEN** `GET /health` 的 `default_workdir` 为 `/opt/projects`(而非主目录或进程 cwd)

#### Scenario: 无配置覆盖且主目录不可解析

- **WHEN** sidecar 无 `server.default_workdir` 配置且无法解析用户主目录
- **THEN** `GET /health` 省略 `default_workdir`(或返回空串)
- **AND** SHALL NOT 报告 sidecar 的启动目录

### Requirement: 文件系统目录枚举端点

服务 SHALL 暴露 `GET /v1/fs/list?path=<dir>&root=<projectRoot>` 端点,枚举
sidecar 文件系统中指定目录的条目。每个条目 SHALL 包含 `name`(文件/目录名)、
`path`(完整路径)和 `isDir`(布尔)。`path` 参数省略时 SHALL 使用枚举根作为
目标目录。

枚举根 SHALL 取自请求的 `root`(被浏览的项目根);`root` 省略时回退到
`default_workdir`。服务 SHALL 将返回的条目限制在该枚举根之内——逃逸该根的路径
SHALL 返回 `403 Forbidden`。该收口 SHALL 跟随请求的根,而非单一的全局
`cfg.DefaultWorkdir`,以便用户在任意位置打开的项目都可浏览。

路径 SHALL 经 `filepath.Clean` + `filepath.EvalSymlinks` 处理。服务 SHALL
拒绝虚拟文件系统前缀(`/proc`、`/sys`、`/dev`、`/run`),返回 `403 Forbidden`。
路径不存在时 SHALL 返回 `404 Not Found`;非目录路径 SHALL 返回 `400 Bad Request`。

枚举 SHALL 为单层(不递归),包含 dotfiles,按文件名排序。服务 SHALL NOT
递归进入子目录。

#### Scenario: 枚举存在的目录

- **WHEN** 客户端 `GET /v1/fs/list?path=/home/user/proj&root=/home/user/proj`
- **THEN** 返回 `200 OK` 和 `{ "path": "/home/user/proj", "entries": [{ "name": "Documents", "path": "/home/user/proj/Documents", "isDir": true }, ...] }`

#### Scenario: 浏览配置全局默认之外的项目

- **WHEN** 已配置全局 `default_workdir = /opt/projects`,客户端浏览另一个项目根
  `GET /v1/fs/list?path=/home/user/other&root=/home/user/other`
- **THEN** 返回 `200 OK`(不返回 `403`),因为收口跟随请求的 `root` 而非全局默认

#### Scenario: 逃逸枚举根的路径被拒绝

- **WHEN** 客户端 `GET /v1/fs/list?path=/etc&root=/home/user/proj`
- **THEN** 返回 `403 Forbidden`

#### Scenario: 省略 path 使用枚举根

- **WHEN** 客户端 `GET /v1/fs/list?root=/home/user/proj`(无 `path`)
- **THEN** 返回 `200 OK`,以 `/home/user/proj` 为枚举根

#### Scenario: 省略 root 回退默认工作目录

- **WHEN** 客户端 `GET /v1/fs/list`(无 `path`、无 `root`)
- **THEN** 返回 `200 OK`,以 `default_workdir` 为枚举根

#### Scenario: 虚拟文件系统路径被拒绝

- **WHEN** 客户端 `GET /v1/fs/list?path=/proc`
- **THEN** 返回 `403 Forbidden`

#### Scenario: 不存在的路径

- **WHEN** 客户端 `GET /v1/fs/list?path=/nonexistent&root=/home/user/proj`
- **THEN** 返回 `404 Not Found`

#### Scenario: 文件路径而非目录

- **WHEN** 客户端 `GET /v1/fs/list?path=/home/user/proj/file.txt&root=/home/user/proj`
- **THEN** 返回 `400 Bad Request`
