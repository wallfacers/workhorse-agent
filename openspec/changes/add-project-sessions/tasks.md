# Tasks: 项目级会话持久化与多活并发

## 0. message 持久化(T1/T4 地基,先做)

- [x] 0.1 `content_json` 序列化:实现 `provider.Message ↔ content_json` 无损
  round-trip,覆盖全部 5 类 block(`text`/`tool_use`/`tool_result`/`thinking` 含
  `signature`/`redacted_thinking`);加 round-trip 单测(Message→json→Message 全等)。
  → `internal/session/transcript.go`(`storedBlock` DTO + `marshalContent`/`unmarshalContent`)。
- [x] 0.2 `Session.AppendMessage` 在 `store != nil && !Ephemeral` 时落库(复刻
  `Emit`/`assignIdx` 判定),消息 id 用 ULID,`created_at` 单调;落库失败记录但不阻断 turn。
  → 排序键是 µs 精度的 `created_at`(ULID 仅同-µs 兜底);失败走 `slog.Error` 不阻断。
- [x] 0.3 压缩一致性:`Session.ReplaceHistory` 在事务里 `DeleteMessages(sessionID)
  + 批量重插`,保证 messages 表 == 模型上下文;补测试覆盖压缩后水合。
  → 新增 `store.Store.ReplaceMessages`(sqlite 用单事务 DELETE+INSERT);ReplaceHistory
  以 per-index µs 偏移保序;fakeStore 同步实现该接口方法。
- [x] 0.4 确认 message 写入触发 `messages_fts`(`session_search` 从此对真实会话
  生效),补一条 FTS 行填充断言。→ `TestMessage_AppendPopulatesFTS` 守护
  AppendMessage→trigger→FTS 组合(trigram MATCH)。

## 1. 持久化 schema 与查询(底座)

- [ ] 1.1 加 migration v3:`sessions` 增 `title TEXT NOT NULL DEFAULT ''`;补
  `idx_sessions_workdir`(按 workdir 列举用);迁移 up/down 与测试。
- [ ] 1.2 确认 sqlite 连接开启 `PRAGMA foreign_keys=ON`,补测试断言 DELETE 级联
  真正清掉 `messages`/`events`/`tool_calls`。
- [ ] 1.3 `store`:新增按 `workdir` 列举(含 `messageCount`/`lastMessagePreview`
  聚合)、`projects` 聚合(distinct workdir + count + max updated_at)、`title`
  读写(`UpdateSession` 覆盖)。

## 2. 持久化水合(T1/T3/T4 核心)

- [ ] 2.1 `Manager`:实现"按需水合"——`GetSession` 未命中内存时回源 store,
  重建 `Session`(state=Idle)并在 `m.mu` 下 double-check 登记,避免并发重复水合。
- [ ] 2.2 水合时调 `ListMessages` 把历史 `provider.Message` 重灌进 agent loop 的
  模型上下文(套用既有 thinking 剥离 / token 计账),实现 T4 续聊真连续。
- [ ] 2.3 `GET …/stream` 与 `POST …/stream` 对"已存在但未加载(含 idle)"会话工作:
  自动触发水合后再挂流 / 收消息。补测试:重启后重开 idle 会话能继续 turn。
- [ ] 2.4 水合失败干净回滚(不残留半个 live 会话);store 错误 → 5xx,不存在/已删
  → 404。

## 3. status 投影与多活并发(T3)

- [ ] 3.1 `statusOf`:6 态 → `idle|running` 投影(`Idle`/`Cancelled`→idle,其余
  →running),集中一处供所有端点复用;持久化未加载会话按 idle。
- [ ] 3.2 验证一个会话的 turn 服务端独立推进,与订阅者数量/有无订阅者无关;
  补多活并发测试(A、B 两会话并发各开 stream,互不干扰,running 在列表可标出)。

## 4. 五个 HTTP 端点 + camelCase 线缆(T2)

- [ ] 4.1 重写会话投影为 camelCase `SessionMeta`(create/get/list/PATCH 统一返回);
  同步改 `internal/api/sessions_test.go` 等现有断言;grep 全仓对旧 snake 字段消费点。
- [ ] 4.2 `GET /v1/sessions?workdir=<path>` → `{ "sessions": [SessionMeta] }`,
  source-of-truth 为 store(重启后仍可列出);无 workdir 时的行为与既有兼容。
- [ ] 4.3 `GET /v1/sessions/{id}/history` → `{ "messages": [HistoryMessage] }`,
  实现 `content_json → parts[]` **翻译**(非原样吐):wire 用 `id`/`name`/`input`/
  `output`/`content`/`text`,**不是**存储的 `toolUseId`/`toolName`;tool_call 按
  `toolUseId` 在 transcript 内 map 回填 output/status;reasoning part **物理带**
  `status:"done"`。加 e2e 断言:assistant 的 `coerceHistory` 裸 cast 后能正确渲染
  text/tool_call/reasoning(字段名错会静默丢)。
- [ ] 4.3a 投影不变量:`SessionMeta.title` 始终输出(可空串不可省略);
  `SessionMeta.status` 严格 ∈ `{idle,running}`(6 态投影后绝不漏出原态)。补单测。
- [ ] 4.4 `PATCH /v1/sessions/{id}` body `{ "title" }` → 更新后的 `SessionMeta`。
- [ ] 4.5 `DELETE /v1/sessions/{id}`:改硬删 + 级联清 transcript;先取消在跑 turn
  再删;返回 2xx 空体。
- [ ] 4.6 `GET /v1/projects` → `{ "projects": [ProjectMeta] }`,只含有未删除会话
  的 workdir。
- [ ] 4.7 在 server.go 注册新路由(`GET ?workdir` 复用既有 `GET /v1/sessions`;
  新增 `GET …/history`、`PATCH /v1/sessions/{id}`、`GET /v1/projects`)。

## 5. title 派生(§8 Q1)

- [ ] 5.1 落第一条 user message 时从首条用户文本派生 `title`(长度截断/单行化),
  写入 `sessions.title`;PATCH 可覆盖;空串语义保留。

## 6. T5 补帧(可选,加分)

- [ ] 6.1 复核 `GET …/stream` 的 `Last-Event-ID`/`last_event_id` + `EventsAfter`
  能让晚加入 running 会话的订阅者补到本轮已发事件;在 spec 固化该语义,补测试。

## 7. 端到端验收(对齐任务书 §10)

- [ ] 7.1 打开项目 P → `GET ?workdir=P` 返回会话列表(含 status)。
- [ ] 7.2 新建会话发几轮 → 重启 sidecar → `GET …/history` 能重建内容。
- [ ] 7.3 多活:A/B 会话并发流式互不干扰,running 在列表标出。
- [ ] 7.4 切回 idle 旧会话发 user_message → 模型带历史上下文续答。
- [ ] 7.5 PATCH 改名后 list 反映新 title;DELETE 后 list 不含该会话且 transcript
  行已删。
- [ ] 7.6 `golangci-lint run` 干净;gofumpt 通过。

## 8. 回填(实现后)

- [ ] 8.1 若实际路由/字段与任务书不同,按 §9 列差异,知会 assistant 同步
  `src-tauri/src/agent/mod.rs` 顶部常量。
