# Agent Wire Protocol v1.1 — 客户端工具执行 & 入站消息重构

> 状态：技术方案（待评审）。基于 v1 协议增量，不破已有契约。
> 实现位置：`internal/server`（Layer 2）+ `internal/tools`（工具注册层）+ `internal/agent`（loop 客户端路径）。
> 前置阅读：[agent-wire-v1.md](agent-wire-v1.md)

---

## 0. 动机：为什么需要客户端工具执行

AgentKit 的部署拓扑决定了 Go 服务端和 Swift 客户端各自拥有**不可跨越的硬件/系统边界**：

| 服务端 (Go/Linux) | 客户端 (Swift/macOS/iOS) |
|---|---|
| `run_command` / `bash` | `AVFoundation` 视频处理 |
| `grep` / `file_read` / `file_write` | `Photos` 相册读写 |
| `git` / `diff` | `HealthKit` 传感器 |
| 网络请求、代码分析 | `UserNotifications` 本地通知 |
| HTTP API 调用 | `NSOpenPanel` 系统权限弹窗 |

**Go 服务端物理上不可能调用 AVFoundation。Swift 客户端不应直接操作服务端文件系统。**

当前 v1 协议是服务端独占工具执行：

```
模型决定调 tool  →  服务端本地执行  →  结果回注 loop
客户端只能旁观（收到 tool_started / tool_finished 渲染卡片）
```

v1.1 新增客户端工具执行路径：

```
模型决定调 tool  →  服务端判断 executor  →  客户端执行  →  tool_result 回传  →  服务端注入 loop
```

本文档定义此路径的完整协议契约、Go 侧类型与实现设计。

---

## 1. 入站消息重构：`agent_input` 统一入站信封

### 1.1 现状

v1 入站消息按 `type` 字段平铺分发：

```json
{ "type": "send_message", "text": "..." }
{ "type": "cancel_turn" }
{ "type": "approval_response", "id": "...", "approved": true }
{ "type": "plan_approval_response", "id": "...", "approved": true }
```

### 1.2 v1.1 新增 `agent_input`

新客户端使用统一的 `agent_input` 信封，以 `kind` 判别消息语义：

```jsonc
// kind: "text" — 用户文本输入（语义等价于旧 send_message）
{
  "type": "agent_input",
  "kind": "text",
  "text": "分析这个项目"
}

// kind: "tool_result" — 客户端工具执行结果回传（execution graph continuation）
{
  "type": "agent_input",
  "kind": "tool_result",
  "tool_result": {
    "tool_use_id": "call_abc123",
    "subtype": "result",
    "content": "视频修剪完成：/tmp/output.mp4",
    "is_error": false
  }
}

// kind: "command" — 系统命令
{
  "type": "agent_input",
  "kind": "command",
  "text": "cancel"       // cancel | switch_model | goal_start（后两者预留）
}

// kind: "system" — 结构化系统指令（预留，v1.1 仅解析 + stub）
{
  "type": "agent_input",
  "kind": "system",
  "command": "patch_context",
  "command_key": "project_rules",
  "command_value": "使用 Swift 6 规范"
}
```

### 1.3 兼容策略

```
┌──────────────────────────────────────────────────────────────────┐
│ send_message 类型继续接受，不删除。                               │
│ 新客户端 → 发 agent_input(kind:"text")                            │
│ 旧客户端 → 继续发 send_message                                   │
│ Router 内部：两种格式路由到同一个 SendMessage() 调用。             │
└──────────────────────────────────────────────────────────────────┘
```

### 1.4 Go 侧类型定义

```go
// internal/server/command.go

// AgentInput 是 v1.1 统一入站信封。
type AgentInput struct {
    Type       string      `json:"type"`                  // "agent_input"
    Kind       string      `json:"kind"`                  // "text" | "tool_result" | "command" | "system"
    Text       string      `json:"text,omitempty"`        // kind="text" | "command"
    ToolResult *ToolResult `json:"tool_result,omitempty"` // kind="tool_result"
    // kind="system" 字段（v1.1 仅解析，语义后补）：
    Command      string `json:"command,omitempty"`
    CommandKey   string `json:"command_key,omitempty"`
    CommandValue string `json:"command_value,omitempty"`
}

// ToolResult 是客户端工具执行结果的结构化负载。
type ToolResult struct {
    ToolUseID string `json:"tool_use_id"`
    Subtype   string `json:"subtype"`            // "result" v1.1；"progress"|"error"|"cancel" v1.2+
    Content   string `json:"content,omitempty"`
    IsError   bool   `json:"is_error,omitempty"`
}
```

---

## 2. 执行权模型：`ExecutionMode` 契约

### 2.1 问题

`tool_started` 加 `executor: "client"` 字段只是**通知**，不构成契约。可能出现：
- 服务端误执行了只有客户端能做的工具（或反过来）
- 客户端无法判断某条 `tool_started` 是否在等自己

### 2.2 方案：在 Tool 注册层声明执行归属

```go
// internal/tools/tool.go

// ExecutionMode 声明工具的执行归属。这是契约——不是建议。
// 服务端 loop 和客户端 dispatcher 都据此做出不可协商的决定。
type ExecutionMode string

const (
    // ExecStrictServer：只能由服务端执行。客户端物理上不可触碰。
    // grep、git、bash、文件操作等归此类。
    ExecStrictServer ExecutionMode = "strict_server"

    // ExecStrictClient：只能由客户端执行。服务端物理上不可执行。
    // AVFoundation、HealthKit、Photos 等归此类。
    ExecStrictClient ExecutionMode = "strict_client"

    // ExecFlex：两端均可执行，默认服务端。v2 可按负载协商。
    // web_search、web_fetch 等纯 HTTP 工具可归此类。
    ExecFlex ExecutionMode = "flex"
)

// ClientTool 是可选接口。实现了它的工具声明自己由客户端执行。
// 服务端 loop 在 tool_started 前检查：
//   - 实现了且 Mode() == ExecStrictClient → 发 executor:"client"，阻塞等待 tool_result
//   - 否则 → 本地 executeTool()
type ClientTool interface {
    ExecutionMode() ExecutionMode
}
```

### 2.3 判断流程（在 agent loop 中）

```
tool_started 前：
  ┌─ tool 实现了 ClientTool 接口？
  │    └─ 否 → 本地执行（默认路径，当前所有行为不变）
  │    └─ 是 → Mode() 是什么？
  │           ├─ strict_server → 本地执行
  │           ├─ strict_client → 发 executor:"client"，Wait() 阻塞等 tool_result
  │           └─ flex           → 本地执行（v1.1 默认服务端，v2 可协商）
```

`wireEvent.Executor` 字段的角色：
- **信息通知**，不是仲裁。它告诉客户端"这条 tool_started 是给你的，请执行并回传"。
- 真正的执行权决定在 tool registry 层的 `ExecutionMode` 契约中完成。
- 客户端收到 `executor: "client"` 的 `tool_started`，从中取 `tool_name` + `tool_args` 调度到本地 tool 实现。
- 客户端收到**没有** `executor: "client"` 的 `tool_started`，只渲染，不执行。

### 2.4 出站事件新增字段

```jsonc
// 当前（服务端执行 — 行为完全不变）
{
  "kind": "tool_started",
  "call_id": "call_42",
  "step": 1,
  "tool_name": "grep",
  "tool_args": { "pattern": "TODO", "path": "./src" }
  // executor 字段缺省 = "server"
}

// 新增（客户端执行 — 新增 executor 字段）
{
  "kind": "tool_started",
  "call_id": "call_99",
  "step": 2,
  "tool_name": "trim_video",
  "tool_args": { "url": "file:///tmp/input.mp4", "start": 0, "duration": 10 },
  "executor": "client"
}
```

Go 侧：`wireEvent` 新增 `Executor string \`json:"executor,omitempty"\``，`toWire` 从 `agent.Event.Executor` 取值。

---

## 3. `tool_result` 的多事件流扩展

### 3.1 问题

当前需求要求 `tool_result` 携带最终结果 `{content, is_error}`。但视频处理、大文件导入等长任务天然有中间状态——进度、部分结果、取消、错误恢复。如果现在把 `tool_result` 定死为"单次最终结果"，v2 必须破协议。

### 3.2 方案：`subtype` 判别字段

在 `ToolResult` 中引入 `subtype` 字段，v1.1 只实现 `"result"`，协议槽位为后续类型预留：

```jsonc
// v1.1：最终结果（唯一实现的 subtype）
{
  "type": "agent_input",
  "kind": "tool_result",
  "tool_result": {
    "tool_use_id": "call_abc123",
    "subtype": "result",
    "content": "视频修剪完成：/tmp/output.mp4",
    "is_error": false
  }
}

// v1.2 预留 — 进度通知
// {
//   "tool_result": {
//     "tool_use_id": "call_abc123",
//     "subtype": "progress",
//     "progress": { "percent": 45, "message": "编码中..." }
//   }
// }

// v1.2 预留 — 结构化错误
// {
//   "tool_result": {
//     "tool_use_id": "call_abc123",
//     "subtype": "error",
//     "error": { "code": "PERMISSION_DENIED", "detail": "用户拒绝了相册访问" }
//   }
// }

// v1.2 预留 — 取消确认
// {
//   "tool_result": {
//     "tool_use_id": "call_abc123",
//     "subtype": "cancel",
//     "reason": "user_aborted"
//   }
// }
```

**前向兼容规则**：
- 服务端收到未知 `subtype` → 按 `"result"` 降级处理，`content` 取 `tool_result.content`。
- 客户端发未知 `subtype` → 服务端不报错，不崩溃。

**v1.1 服务端只实现 `"result"` 的处理**。`"progress"` 到来时服务端会将其视同结果并解除 `Wait()` 阻塞——客户端不应在 v1.1 发 progress（发也不崩，但语义不对）。待 v1.2 服务端支持 `ClientToolWaiter` 的 progress→lease 续期后，客户端再启用 progress。

---

## 4. 分布式阻塞生命周期：Timeout / Lease / Cancellation

### 4.1 问题

`RemoteApprover` 有 `deadline_ms`（人类决策，秒级），但 `tool_result` 面对的是机器执行——视频处理可能数分钟，需要更完整的生命周期管理：

- **Lease timeout**：客户端断线或卡死后，服务端不能永久阻塞
- **Cancellation propagation**：服务端 cancel turn 时，正在客户端执行的工具应停止
- **Progress → lease 续期**：长任务发进度，隐式续租（v1.2）

### 4.2 ClientToolWaiter 接口

```go
// internal/server/tool_waiter.go

// ClientToolWaiter 管理委托给客户端的工具调用的完整生命周期。
// 形状与 RemoteApprover 完全一致：Wait() 阻塞在 turn goroutine，
// Deliver() 由 Router 在 WS read-loop goroutine 中调用。
type ClientToolWaiter interface {
    // Wait 阻塞直到客户端回传结果、lease 超时、或上下文取消。
    // leaseTimeout：客户端必须在此时间内至少回传一次（v1.2+ 发 progress 可续期）。
    // 返回 (result, nil) 正常；返回 ("", ctx.Err()) 表示 turn 被取消；
    // 返回 ("tool error: ...", nil) 表示 lease 超时。
    Wait(ctx context.Context, callID string, leaseTimeout time.Duration) (ToolCallResult, error)

    // Deliver 由 Router 调用，将客户端回传的结果注入对应的 Wait 调用。
    // callID 匹配 tool_started 的 call_id。如果 callID 未被 Wait 注册（已超时/已完成），
    // 静默丢弃。
    Deliver(callID string, result ToolCallResult)

    // CancelAll 在 WS 断开或 session 销毁时调用。所有 pending Wait 立即返回
    // context.Canceled，保证 agent loop 不会永久卡死。
    CancelAll()
}

type ToolCallResult struct {
    Subtype string // "result" (v1.1)
    Content string
    IsError bool
}
```

### 4.3 RemoteToolResultWaiter 实现

```go
// internal/server/tool_waiter.go

type RemoteToolResultWaiter struct {
    mu      sync.Mutex
    pending map[string]*pendingCall
}

type pendingCall struct {
    ch   chan ToolCallResult
    done chan struct{} // closed when Wait returns (prevents double-deliver)
}

func NewRemoteToolResultWaiter() *RemoteToolResultWaiter {
    return &RemoteToolResultWaiter{pending: make(map[string]*pendingCall)}
}

func (w *RemoteToolResultWaiter) Wait(ctx context.Context, callID string, leaseTimeout time.Duration) (ToolCallResult, error) {
    pc := &pendingCall{
        ch:   make(chan ToolCallResult, 1),
        done: make(chan struct{}),
    }
    w.mu.Lock()
    w.pending[callID] = pc
    w.mu.Unlock()

    defer func() {
        close(pc.done)
        w.mu.Lock()
        delete(w.pending, callID)
        w.mu.Unlock()
    }()

    // v1.1: lease 超时即失败。v1.2: Deliver 收到 progress 时重置 timer。
    timer := time.NewTimer(leaseTimeout)
    defer timer.Stop()

    select {
    case <-ctx.Done():
        return ToolCallResult{}, ctx.Err()
    case <-timer.C:
        return ToolCallResult{Subtype: "result", Content: "tool error: client timeout", IsError: true}, nil
    case r := <-pc.ch:
        return r, nil
    }
}

func (w *RemoteToolResultWaiter) Deliver(callID string, result ToolCallResult) {
    w.mu.Lock()
    pc, ok := w.pending[callID]
    w.mu.Unlock()
    if !ok {
        return // 静默丢弃：callID 已超时/已完成
    }
    select {
    case <-pc.done:
        return // Wait 已返回（超时/取消），不阻塞 Deliver 方
    case pc.ch <- result:
    }
}

func (w *RemoteToolResultWaiter) CancelAll() {
    w.mu.Lock()
    defer w.mu.Unlock()
    for _, pc := range w.pending {
        select {
        case <-pc.done:
        default:
            close(pc.done) // 不会直接发 cancel——Wait 的 defer 会清理
        }
    }
    w.pending = nil
}
```

### 4.4 生命周期时序

```
正常路径：
  Server: tool_started(executor:"client", call_id="call_99", ...)
          → t = 0, Wait(ctx, "call_99", 120s)
  Client: [执行 AVFoundation.trim_video()]
  Client: → agent_input(kind:"tool_result", tool_result:{tool_use_id:"call_99", subtype:"result", ...})
          → t = 45s, Router.Deliver("call_99", ...)
  Server: Wait() 返回 (result, nil)
          → 继续 agent loop

超时路径：
  Server: tool_started(executor:"client", call_id="call_99", ...)
          → t = 0, Wait(ctx, "call_99", 120s)
  Client: [崩溃/断线]
          → t = 120s, timer 触发
  Server: Wait() 返回 ("tool error: client timeout", nil)
          → agent loop 将超时视为工具错误，继续下一步（或模型决定如何处理）

取消路径：
  Server: tool_started(executor:"client", call_id="call_99", ...)
          → t = 0, Wait(ctx, "call_99", 120s)
  Client: [执行中...]
  Server: [用户发 cancel_turn] → turnCtx.Cancel()
          → t = 10s, ctx.Done()
  Server: Wait() 返回 ("", context.Canceled)
          → agent loop 停止该 turn
  Client: [WS 仍在 → 下一次 tool_result 的 call_id 不在 pending map → 静默丢弃]
          [WS 断开 → CancelAll() → 所有 pending 清理]

v1.2 续期路径（预留）：
  Server: tool_started(executor:"client", call_id="call_99", ...)
          → t = 0, Wait(ctx, "call_99", 120s), timer = 120s
  Client: → tool_result(subtype:"progress", ...)
          → t = 60s, Router.Deliver("call_99", {Subtype:"progress"})
  Server: Wait 检测到 Subtype=="progress" → timer.Reset(120s)
          → lease 续期
  Client: → tool_result(subtype:"result", ...)
          → t = 180s, Router.Deliver("call_99", {Subtype:"result", ...})
  Server: Wait() 返回 (result, nil)
```

### 4.5 取消传播方向

```
Server → Client 取消（v1.1 机制）：
  turn 被 cancel → turnCtx.Done() → Wait() 返回 context.Canceled
  → agent loop 退出该 turn
  → WS 如果还在，客户端下次发 tool_result 时 call_id 已不在 pending map
  → 静默丢弃

  WS 断开 → CancelAll() → 所有 pending Wait 感知
  → agent loop 将取消视同工具错误

Client → Server 取消（v1.2 预留）：
  客户端用户中止 → tool_result(subtype:"cancel")
  → Wait 返回 cancel 结果 → agent loop 继续
```

---

## 5. Agent Loop 改动

### 5.1 当前 loop 结构（简化）

```go
// internal/agent/loop.go — RunTurn()
for step := 0; step < maxSteps; step++ {
    resp := model.Complete(messages)
    if !resp.HasToolCalls() {
        // 完成 → 返回
    }
    for _, call := range resp.ToolCalls {
        emit(EventToolStarted{call.ID, call.Name, call.Args})
        observation, err := executeTool(ctx, tool, call.ID, input)  // 本地执行
        emit(EventToolFinished{...})
        messages.append(tool_result)
    }
}
```

### 5.2 v1.1 改动后的 loop

```go
for step := 0; step < maxSteps; step++ {
    resp := model.Complete(messages)
    if !resp.HasToolCalls() {
        // 完成 → 返回（不变）
    }
    for _, call := range resp.ToolCalls {
        // 查工具的执行归属
        executor := executorFor(call.Function.Name)

        if executor == "client" {
            // ──── 客户端执行路径 ────
            emit(EventToolStarted{call.ID, call.Name, call.Args, Executor: "client"})

            result, waitErr := r.clientWaiter.Wait(ctx, call.ID, r.clientToolTimeout())
            if waitErr != nil {
                observation = "Tool error: " + waitErr.Error()
            } else if result.IsError {
                observation = "Tool error: " + result.Content
            } else {
                observation = result.Content
            }
        } else {
            // ──── 服务端执行路径（行为完全不变）────
            emit(EventToolStarted{call.ID, call.Name, call.Args})

            observation, execErr = r.executeTool(ctx, tool, call.ID, input)
            if execErr != nil {
                observation = "Tool error: " + execErr.Error()
            }
        }

        // —— 两条路径在此汇合 ——
        // Observation 富化、truncation、todo/skill telemetry 不变
        emit(EventToolFinished{call.ID, call.Name, observation, ...})
        sess.Messages = append(sess.Messages, model.Message{
            Role: model.RoleTool, ToolCallID: call.ID, Content: observation,
        })
    }
}
```

### 5.3 Runner 新增字段

```go
type Runner struct {
    // ... 已有字段 ...

    // ClientWaiter 阻塞等待客户端回传工具结果。nil 时所有工具本地执行（默认）。
    ClientWaiter ClientToolWaiter

    // ClientToolTimeout 是单个客户端工具调用的 lease 超时。
    // 零值时使用默认值（2 分钟）。
    ClientToolTimeout time.Duration
}

// executorFor 判断工具的执行归属。
func (r *Runner) executorFor(toolName string) string {
    tool, ok := r.Tools.Get(toolName)
    if !ok {
        return "" // 未知工具：服务端自己报错
    }
    if ct, ok := tool.(ClientTool); ok && ct.ExecutionMode() == ExecStrictClient {
        if r.ClientWaiter != nil {
            return "client"
        }
        // waiter 未连接 → 没有客户端可以执行 → 返回错误
    }
    return "" // 空 = 服务端执行（默认）
}

func (r *Runner) clientToolTimeout() time.Duration {
    if r.ClientToolTimeout > 0 {
        return r.ClientToolTimeout
    }
    return 2 * time.Minute
}
```

---

## 6. Session / Transport / WS Handler 改动

### 6.1 Session 接口新增方法

```go
// internal/server/bridge.go

type Session interface {
    Subscriber
    CommandTarget
    SetApprover(agent.Approver)
    SetPlanApprover(agent.PlanApprover)
    SetClientToolWaiter(ClientToolWaiter) // 新增
}
```

### 6.2 TransportSession 实现

```go
// internal/conversation/transport.go

func (s *TransportSession) SetClientToolWaiter(w ClientToolWaiter) {
    s.ex.SetClientToolWaiter(s.id, w)
}
```

### 6.3 WS Handler 生命周期

```go
// internal/server/ws.go — ServeHTTP 内

waiter := NewRemoteToolResultWaiter()
sess.SetClientToolWaiter(waiter)
defer func() {
    waiter.CancelAll()               // 断开时唤醒所有 pending Wait
    sess.SetClientToolWaiter(nil)    // 恢复 nil（后续 turn 无客户端执行能力）
    approver.Close()
    sess.SetApprover(denyApprover{})
    sess.SetPlanApprover(nil)
}()
```

### 6.4 Router 新增 tool_result 分发

```go
// internal/server/router.go

type ToolResultResolver interface {
    Deliver(callID string, result ToolCallResult)
}

type Router struct {
    Commands    CommandTarget
    Approvals   ApprovalResolver
    ToolResults ToolResultResolver // 新增
}

func (r Router) Route(ctx context.Context, data []byte) {
    var env struct {
        Type string `json:"type"`
    }
    if json.Unmarshal(data, &env) != nil {
        return
    }
    switch env.Type {
    case "send_message":
        // ... 现有逻辑不变 ...
    case "agent_input":
        var m AgentInput
        if json.Unmarshal(data, &m) != nil {
            return
        }
        switch m.Kind {
        case "text":
            if r.Commands != nil {
                go func() { _, _ = r.Commands.SendMessage(ctx, m.Text) }()
            }
        case "tool_result":
            if m.ToolResult != nil && r.ToolResults != nil {
                r.ToolResults.Deliver(m.ToolResult.ToolUseID, ToolCallResult{
                    Subtype: m.ToolResult.Subtype,
                    Content: m.ToolResult.Content,
                    IsError: m.ToolResult.IsError,
                })
            }
        case "command":
            if m.Text == "cancel" && r.Commands != nil {
                r.Commands.Cancel()
            }
            // switch_model、goal_start 预留
        case "system":
            // v1.1 stub：解析但不执行
            // patch_context、update_memory、override_plan 语义后补
        }
    case "cancel_turn":
        // ... 现有逻辑不变 ...
    case "approval_response":
        // ... 现有逻辑不变 ...
    case "plan_approval_response":
        // ... 现有逻辑不变 ...
    }
}
```

---

## 7. Capabilities 声明（hello 帧）

### 7.1 新增字段

```json
{
  "type": "hello",
  "protocol_version": 1,
  "server": "codeagent/deepseek-v4",
  "capabilities": ["streaming", "thinking", "tool_streaming", "plan_mode", "subagents", "session_resume", "client_tool_execution"]
}
```

### 7.2 能力清单

| 能力 | 说明 | v1 状态 |
|---|---|---|
| `streaming` | 支持 `token_delta` 流式输出 | ✅ |
| `thinking` | 支持 `thinking` 推理文本事件 | ✅ |
| `tool_streaming` | 支持 `tool_stdout` / `tool_stderr` 实时输出 | ✅ |
| `plan_mode` | 支持 propose_plan / plan_approval_request 流程 | ✅ |
| `subagents` | 支持 `task_started` / `task_finished` 子代理事件 | ✅ |
| `session_resume` | 支持 `GET /events` 恢复历史 | ✅ |
| `client_tool_execution` | 支持 `executor:"client"` + `tool_result` 回传 | ✅ v1.1 |
| `image_input` | 图能力 | ❌ 未实现 |

### 7.3 Go 侧改动

```go
// internal/server/encoder.go

type helloFrame struct {
    Type            string   `json:"type"`
    ProtocolVersion int      `json:"protocol_version"`
    Server          string   `json:"server,omitempty"`
    Capabilities    []string `json:"capabilities,omitempty"`
}

func Hello(server string, capabilities []string) ([]byte, error) {
    return json.Marshal(helloFrame{
        Type:            "hello",
        ProtocolVersion: protocolVersion,
        Server:          server,
        Capabilities:    capabilities,
    })
}
```

`Bridge.Run` 和调用方需传递 capabilities 列表。默认能力集在 `WSHandler` 或 `MuxOptions` 中配置。

---

## 8. Session Ownership Model（文档化，零代码改动）

以下行为是 v1 已有实现，v1.1 在文档中显式声明：

| 概念 | 语义 | 实现位置 |
|---|---|---|
| `POST /v1/conversations` | 创建 server-owned runtime session | `mux.go:100-113` |
| `ConversationRef.id` | server-assigned execution context UUID，客户端不生成 | `sqlite_repository.go` |
| `GET .../stream` | attach 到已有 session | `mux.go:195-209` |
| WS disconnect | 释放传输绑定，session 数据持久在 SQLite；approver 恢复 deny-all；waiter 所有 pending 被 CancelAll 唤醒 | `ws.go:94-98` |

---

## 9. 实现计划

### P0（本期 v1.1）

| 任务 | 文件 | 量级 |
|---|---|---|
| `AgentInput` + `ToolResult` 类型定义 | `command.go` | ~30 行 |
| `agent_input` 4 种 kind 的 Router 分发（`system` 仅 stub） | `router.go` | ~30 行 |
| 旧 `send_message` 兼容不变 | 无 | 零改动 |
| `helloFrame` 加 `capabilities` 字段 | `encoder.go` | ~10 行 |
| Golden 测试更新 | `testdata/messages/` | ~2 个新文件 |

### P1（下期 — 客户端工具执行）

| 任务 | 文件 | 量级 |
|---|---|---|
| `ExecutionMode` + `ClientTool` 接口 | `tools/tool.go` | ~15 行 |
| `agent.Event.Executor` + `wireEvent.Executor` + `toWire` 映射 | `event.go`、`wire.go` | ~10 行 |
| `ClientToolWaiter` 接口 + `RemoteToolResultWaiter` 实现 | `tool_waiter.go`（新） | ~80 行 |
| `Session` 接口 + `TransportSession` + `ActiveTurnRegistry` 新增方法 | `bridge.go`、`transport.go`、`activeturn.go` | ~25 行 |
| Router 新增 `ToolResults` 字段 + `tool_result` 分发 | `router.go` | ~15 行 |
| Agent loop 客户端路径 + `clientWaiter` 字段 | `loop.go` | ~30 行 |
| WS Handler 生命周期（挂/摘 waiter） | `ws.go` | ~10 行 |
| `RunBuilder`/`RuntimeContext` 注入 waiter | `runtime.go` | ~10 行 |
| Golden 测试更新 | `testdata/` | ~3 个新文件 |

P1 总计约 **200 行 Go 代码**，分布在 ~10 个文件中。核心复杂度在 agent loop 的条件分支和超时/取消生命周期测试。

---

## 10. 协议兼容性

- **v1 客户端连 v1.1 服务端**：`hello.capabilities` 是可选字段（v1 客户端忽略未知字段）；`executor` 字段同理。行为完全不变。
- **v1.1 客户端连 v1 服务端**：`hello` 没有 `capabilities` → 客户端按能力全部缺失处理（不依赖 `client_tool_execution`）。发 `agent_input` → v1 服务端 Router 命中 `default` 分支 → 忽略（`route.go` 已有此行为："Unknown message types are ignored"）。
- **同一 major 版本内只增不改**：新增 `kind`、新增可选字段、新增 `subtype`。不删除、不重命名、不改语义。
- **客户端必须忽略未知 `kind` 和未知字段**（前向兼容）。
- **Golden 文件锁定所有契约变更**。

---

## 附录 A：完整的客户端工具执行时序

```
1. [macOS] POST /v1/conversations → { id: "sess_abc" }
2. [macOS] 连接 ws://.../sess_abc/stream
3. [macOS] ← hello { capabilities: [..., "client_tool_execution"] }
4. [macOS] → agent_input(kind:"text", text:"修剪这个视频")
5. [macOS] ← turn_started(text:"修剪这个视频")
6. [macOS] ← model_started, token_delta × N, model_finished
7. [macOS] ← tool_started(call_id:"call_99", tool_name:"trim_video",
                          tool_args:{url:"...", start:0, duration:10},
                          executor:"client")
          ⏸ 服务端 agent loop 阻塞在 Wait("call_99", 120s)
8. [macOS] 检测 executor=="client" → 调度到本地 AVFoundation 实现
9. [macOS] AVFoundation.trim_video() 执行中...
10.[macOS] → agent_input(kind:"tool_result", tool_result:{
               tool_use_id:"call_99", subtype:"result",
               content:"修剪完成: /tmp/output.mp4", is_error:false })
11.[macOS] ← tool_finished(call_id:"call_99", observation:"修剪完成: ...")
          ▶ 服务端 agent loop 恢复
12.[macOS] ← turn_finished(text:"视频已修剪完成")
```

## 附录 B：与 RemoteApprover 的架构对比

```
                      RemoteApprover              RemoteToolResultWaiter
                      ─────────────               ──────────────────────
目的                   人类审批决策                 客户端工具执行结果
平面                   control-plane               data-plane（通过入站消息回传）
阻塞方法               Approve(tool, input) bool   Wait(ctx, callID, timeout) (result, error)
唤醒方法               Resolve(id, approved)       Deliver(callID, result)
失败默认               deny (false)                timeout → tool error
连接断开               denyApprover 恢复            CancelAll() 唤醒所有 pending
超时单位               deadline_ms（秒级）          lease timeout（分钟级）
续期机制               无（人类决策不续期）         progress 事件续 lease（v1.2）
per-request 隔离       pending map + channel        pending map + channel
goroutine 模型         turn goroutine 调 Approve    turn goroutine 调 Wait
                       WS read-loop 调 Resolve      WS read-loop 调 Deliver
```

二者共享完全相同的并发模型，差异仅在语义层。
