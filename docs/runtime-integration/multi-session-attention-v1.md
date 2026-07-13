# Multi-session Attention v1

`Multi-session Attention`（多会话活动、审批与完成提醒）是多会话执行的独立子里程碑。它保证未连接或未选中的会话仍可通过全局快照被 GUI 可靠发现。

## Capabilities

Runtime 分别声明执行能力与注意力快照能力：

```json
{
  "multi_session_execution_v1": true,
  "session_attention_snapshot_v1": true,
  "session_attention_delta_v1": true
}
```

`session_attention_snapshot_v1` 仅在 Runtime 同时具备可靠 terminal 持久化、唯一取消终态、session-scoped broker 计数和稳定 sequence 时开启。`session_attention_delta_v1` 表示客户端可用顶层 `cursor` 请求增量快照。它们不表示 Runtime 保存 GUI 已读状态，也不表示 Runtime 管理系统通知。

## Activity snapshot

`GET /v1/activity` 保留原有字段，并返回：

```json
{
  "generated_at": "2026-07-13T12:00:00Z",
  "cursor": 20917,
  "is_delta": false,
  "sessions": [
    {
      "session_id": "session_a",
      "turn_id": "turn_a",
      "active_turn_id": "turn_a",
      "state": "waiting_approval",
      "queue_position": 0,
      "pending_approval_count": 1,
      "pending_client_tool_count": 0,
      "last_sequence": 20917,
      "latest_terminal": {
        "turn_id": "turn_previous",
        "kind": "turn_finished",
        "sequence": 20890,
        "at": "2026-07-13T11:58:00Z"
      },
      "updated_at": "2026-07-13T12:00:00Z"
    }
  ]
}
```

首次加载调用 `GET /v1/activity` 获取完整基线。之后调用
`GET /v1/activity?since_sequence=20917`，Runtime 返回：

- `last_sequence > since_sequence` 的会话最新 attention head；
- 不论游标是否变化，仍然返回 queued/running/resuming/paused 或 broker pending 的会话；
- 顶层 `cursor` 为读取事务看到的事件库最大 sequence；
- `is_delta=true`，客户端必须合并而非替换本地快照。

因此周期轮询不会随历史会话数量增长。未知、负数或非整数游标返回 `400`。旧客户端不传游标时仍获得完整快照。

迁移期 `turn_id` 始终等于 `active_turn_id`。`latest_terminal` 只来自已持久化的 `turn_finished`、`turn_failed` 或 `turn_cancelled`；`turn_paused` 不是 terminal。

## Projection rules

活动状态优先级：

```text
waiting_approval
→ waiting_client_tool
→ queued
→ running / resuming
→ paused
→ idle
```

若 `latest_terminal.turn_id == active_turn_id`，terminal 状态优先，不能继续报告 running。若 turn ID 不同，active turn 正常展示，同时保留上一轮 terminal。

持久化的 `running` / `resuming` 只是恢复检查点，不能单独证明当前进程仍在执行。只有 scheduler 中存在相同 session 的 live turn 时才能报告 running/resuming；否则投影必须降级为 `paused`，避免进程内异常退出后产生无法取消的僵尸运行态。

审批 verdict 和 client-tool result 必须先通过 session control revision 校验。有效结果按“从 pending 投影移除，再唤醒等待 goroutine”的顺序处理。旧连接、重复结果和未知 request ID 不改变 attention。快照只包含计数；请求内容仍通过定向 session channel 传输。

## Reliability and ownership

- terminal append 失败必须返回 executor，且未持久化 terminal 不得以 sequence 0 推送为可靠完成；
- queued cancel 与 running cancel 均只持久化一个 `turn_cancelled`；
- Runtime 只提供稳定 terminal sequence，不保存已读游标；
- GUI 负责维护 `session_id → last_seen_terminal_sequence` 并决定提醒、已读和系统通知；
- v1 使用 HTTP 快照校准与 session WebSocket 实时事件，不要求全局 activity SSE。
