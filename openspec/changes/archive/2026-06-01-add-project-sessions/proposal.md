# Proposal: 项目级会话持久化与多活并发(对接 assistant 的 add-project-sessions)

## Why

`workhorse-assistant`(Tauri/TS 桌面端)要落地 Claude Code / opencode 式的工作
模型:**项目 = 一个本地路径(创建会话时传的 `workdir`)**,一个项目下挂多个会话,
用户可在会话间切换、重命名、删除,切走的会话继续在后台跑。assistant 侧的 Rust 桥
**已实现并在调用**下列五个端点(已过编译与单测),只等 sidecar(本仓)补齐端点与
持久化,端到端回路即可闭合。契约见 assistant 仓
`openspec/changes/add-project-sessions/workhorse-agent-tasks.md`(字段一律 camelCase)。

底层部分就位但有两处关键缺口。`internal/store/sqlite` 已把 **`sessions` 与
`events`** 两表持久化(`CreateSession`/`AppendEvent`,events 的 idx 即 SSE `id:`);
但 **`messages` 表在生产代码里从未被写入**——`store.AppendMessage` 零生产调用,
agent loop 只把对话追加进内存 `s.history`。两处缺口因此叠加:

1. **transcript 不落盘**:进程重启后 `s.history` 蒸发,`messages` 表始终为空,
   `GET …/history` 与 T4 续聊都没有数据源。events 表虽是权威审计日志,却**无法**
   替代——thinking 块的 `signature` 按设计**不进 SSE**(故不进 events),从 events
   重建的 Message 缺签名会被 Anthropic round-trip 拒绝,T4 对开 thinking 的会话破功。
   所以本变更必须新增**真正的 message 持久化**(带 signature 的无损 round-trip)。
2. **会话不水合**:`internal/session/manager.go` 的 `ListSessions()` 与 `GetSession()`
   只认内存 `m.sessions`(注释自陈 "Group 9 will" 水合,一直延后),进程重启后持久化
   会话查不到、`GET …/stream` 无法重开"已存在但未加载"的 idle 会话。

本变更补齐 message 持久化 + 会话水合,并加上五个端点的线缆映射。

## What Changes

- **message 持久化(T1/T4 地基,本变更最大的一块)**:让 `Session.AppendMessage`
  在 `store != nil && !Ephemeral` 时落库(复刻 `Emit` 的判定),定义 `content_json`
  序列化无损 round-trip 全部 5 类 block(`text`/`tool_use`/`tool_result`/`thinking`
  含 **signature**/`redacted_thinking`)。同步处理压缩:`ReplaceHistory` 需重写持久化
  以免 messages 表与模型上下文漂移。副作用:激活 `messages_fts`,让 `session_search`
  对真实会话生效(当前是悬空的潜在 bug)。
- **持久化水合(T1/T3/T4)**:`Manager` 在 `GetSession`/列举/`GET|POST …/stream`
  命中"持久化但未加载"的会话时,从 store 回源**水合**出 `Session`,并用 `ListMessages`
  把历史灌回 agent loop 的模型上下文(让续聊在模型记忆层面也连续,不只 UI 连续)。
- **五个端点(T2)**:
  - `GET /v1/sessions?workdir=<path>` — 按项目列出会话(source-of-truth 为 store)。
  - `GET /v1/sessions/{id}/history` — 返回 transcript,`messages[].parts[]` 形状。
  - `PATCH /v1/sessions/{id}` — 重命名(`{ "title" }`)。
  - `DELETE /v1/sessions/{id}` — 改为**硬删 + 级联清 messages/events**(对齐
    Claude Code / opencode:删会话即删其 transcript)。
  - `GET /v1/projects` — 由非删除会话的 `workdir` 派生项目列表。
- **线缆形状统一为 camelCase**:新增 `SessionMeta`(含 `status`、`title`、
  `messageCount`、`lastMessagePreview`)、`HistoryMessage`、`ProjectMeta`。为追求
  一致性,**把现有会话相关端点(create/get/list)的响应也统一为 camelCase**,
  消除同一 API 两种命名风格的长期债(assistant 对现有端点仅读 `.id`,统一不破坏它)。
- **status 映射**:6 态状态机(`Idle/Thinking/AwaitPerm/Executing/Compacting/Cancelled`)
  对外投影为 `idle|running`——`Idle`/`Cancelled`→`idle`,其余→`running`。
- **title 派生**:从首条用户消息派生(§8 开放问题1 选定方案);需新增 `sessions.title`
  列(schema migration v3)。
- **SSE 补帧(T5,可选)**:`events` 表 + `EventsAfter`/`MaxEventIdx` 与现有
  `Last-Event-ID` 支持已基本满足;本变更复核并在 spec 中固化重开订阅者的补帧语义。

## Capabilities

### Modified Capabilities

- `session-management`:会话列举改为以持久化为 source-of-truth、支持按 `workdir`
  分桶;新增重启后水合、重开 idle 会话、重命名、硬删级联、项目派生、title 派生。
- `api-protocol`:HTTP 端点集合新增 list-by-workdir / history / PATCH / projects;
  会话线缆形状统一为 camelCase 的 `SessionMeta`/`HistoryMessage`/`ProjectMeta`;
  DELETE 语义由软删改为硬删 + 级联。

## Impact

- **Code**:`internal/session/session.go`(`AppendMessage`/`ReplaceHistory` 落库 +
  `content_json` 序列化)、`internal/session/manager.go`(水合)、
  `internal/api/sessions.go`(五端点 + camelCase 投影)、
  `internal/api/stream_get.go`/`stream_post.go`(重开 idle 会话)、
  `internal/store/sqlite/migrations.go`(v3 加 `title` 列)、
  `internal/store/store.go`/`crud.go`(按 workdir 查询、硬删级联、projects 聚合、
  压缩时删旧 messages)、`internal/agent/loop.go`(水合时从 `ListMessages` 重建上下文)。
- **Cross-repo**:闭合 assistant `add-project-sessions` 依赖的契约;assistant 桥
  常量集中在 `src-tauri/src/agent/mod.rs` 顶部,若最终字段/路由有出入按任务书 §9
  『回填』同步。
- **Backward compatibility**:会话端点响应由 snake_case 切到 camelCase 是**破坏性**
  变更——需同步更新 `internal/api/sessions_test.go` 等现有断言;DELETE 由软删改硬删
  会真正清除历史(产品语义上正是预期)。
- **Out of scope(本轮不做,见任务书 §7)**:跨主机 `workdir` 翻译、WSL 远程的
  `GET /v1/fs/list` 文件枚举端点、history 分页(当前一次性拉全量)、把"注册过但零
  会话"的路径纳入 `/v1/projects`。
