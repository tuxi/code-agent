# 后台任务恢复协议

协议版本仍为 agent-wire v1。此扩展与 `background_job_control_v1` 独立，配置默认开启，实际启用仍要求宿主能力与双端协商。

`agent.background_job_notifications` 支持既有用户全局配置及工作区配置。未配置时为 true；用户全局配置可设 false 关闭，工作区显式值可覆盖全局值。无需逐个工作区添加 true。默认值只在配置合并后解析，不把缺省层当作显式 true。

## 协商

只有当前会话具备持久通知、continuation 端口，且工作区的 `agent.background_job_notifications` 开启时，hello 才动态追加 `background_job_notifications_v1`。

新 AgentKit 在每次会话 hello 后声明：

```json
{"type":"client_capabilities","capabilities":["background_job_notifications_v1"]}
```

声明独立于 register_tools。服务端核对当前连接修订号、配置和存储可读性后设置宿主能力，返回：

```json
{
  "type":"background_continuation_ready",
  "enabled":true,
  "turn_id":"turn_example",
  "background":{
    "turn_id":"turn_example",
    "state":"waiting",
    "revision":2,
    "job_ids":["job_example"]
  }
}
```

没有检查点时省略 turn_id/background；未授权或状态读取失败时 enabled=false。enabled 表示当前连接的宿主能力，既有后台状态快照不等于重新启动授权。最终是否提供 yield 还取决于该 Turn 的运行器、凭据和审批边界。

新连接先撤销旧能力，只有最新连接可重新授予。旧连接断开不撤销新连接授权。当前连接断开撤销自动继续能力，不等同于用户停止，也不取消进程。

## 事件与回放

持久生命周期事件使用以下 kind，并携带上述 background 对象：

| kind | state | 含义 |
| --- | --- | --- |
| turn_waiting_background | waiting | 检查点成功保存，等待完成通知 |
| turn_background_resumed | resuming | 已取得执行许可，继续原 Turn |
| turn_background_blocked | needs_attention | 不能安全恢复，可选 reason 描述原因 |

重连快照还可能返回 ready、stopped、finished。无法确认实际执行的 active 检查点按 needs_attention 展示。revision 是同一 Turn 的持久等待修订号，不是事件 seq；一次继续不增加新的人类输入，不补发 turn_started。

快照先经过客户端现有 backfill 缓冲。客户端按 Turn ID、revision 和阶段拒绝过期更新：同修订号不能从 resuming 回到 waiting；新修订号可再次等待；已结束或被新输入替换的工作不能复活。终态仍沿用 turn_finished/turn_failed/turn_cancelled，断开中断沿用 turn_paused。

公开载荷仅包含生命周期事实。检查点、运行器绑定、凭据和原始命令输出不包含在 background 对象中；命令与日志仍使用既有 Job 查询接口。

## 停止

```json
{"type":"cancel_turn","turn_id":"turn_example"}
```

显式 turn_id 先建立持久停止栅栏，再取消匹配的恢复/执行。旧目标、空值、null、无效目标或不支持目标取消的宿主不能退化为取消当前新工作。客户端等待真实生命周期确认；超时是待处理状态，不能假称已取消。

不带 turn_id 的旧消息继续保留既有“取消当前活跃执行”语义，不扩大成取消历史等待。停止指定进程仍使用独立 Job 停止接口；它的真实终态可唤醒尚未被停止的等待工作。
