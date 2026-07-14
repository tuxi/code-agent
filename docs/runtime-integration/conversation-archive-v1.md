# Conversation Archive/Restore v1

## Capability

Runtime 仅在当前会话存储后端支持持久化归档时声明：

```json
{
  "conversation_archive_v1": true
}
```

自定义存储未实现归档扩展时，capability 必须为 `false`，归档接口返回
`501 conversation_archive_not_supported`。客户端不得在本地伪造归档状态。

## API

```text
POST /v1/conversations/{session_id}/archive
POST /v1/conversations/{session_id}/restore
GET  /v1/conversations?archived=false
GET  /v1/conversations?archived=true
```

未提供 `archived` 时等同于 `archived=false`。参数只接受字面值
`true` 或 `false`。

归档成功：

```json
{
  "id": "session_a",
  "archived_at": "2026-07-14T10:00:00Z"
}
```

恢复成功：

```json
{
  "id": "session_a"
}
```

列表、详情和 activity 中可携带 `archived_at`。归档与恢复均幂等；重复归档
返回首次归档的稳定时间。

## Safety semantics

archive、restore、delete 和 turn acceptance 使用同一个 session operation gate。
以下状态拒绝归档或破坏性操作：

```text
accepted
queued
running
resuming
waiting_approval
waiting_client_tool
paused
```

响应为：

```json
{
  "code": "conversation_in_use",
  "session_id": "session_a",
  "state": "queued"
}
```

HTTP status 为 `409`。Runtime 已提交 archive 后，旧连接或旧 session handle
不能启动新 turn；恢复完成后才能再次执行。

## Persistence and ownership

- `archived_at` 是 Runtime 持久化事实，重启后不得丢失。
- 普通 whole-session Save 不得用旧快照清除已提交的归档状态。
- 归档不删除消息、事件、terminal sequence 或未读事实。
- 归档不删除、移动或清理 Managed Worktree。
- 删除已归档 conversation 仍不隐式删除 Worktree；清理由显式 dirty-safe
  Worktree remove 协议负责。
- restore 只改变列表归属，不重写会话更新时间、事件或 Worktree 状态。

