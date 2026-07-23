# Flux Dynamic DAG 与 Code-Agent 集成设计 v1

状态：设计已确认，Phase 0/1/2/3 与 Phase 4 snapshot API 已实施，客户端 DAG UI 联调中  
涉及仓库：`code-agent`、`flux`、`flux-workflow`  
目标读者：Runtime、Flux、iOS/macOS 客户端开发者

## 1. 背景

`code-agent` 当前以 Agent Loop 驱动模型逐步选择工具。它适合探索性任务，但复杂任务的执行路径会随模型每一步判断而变化。Flux 可以先把目标生成、校验为完整 DAG，再交给确定性 Workflow Engine 执行，适合有依赖、并行、异步等待和恢复要求的任务。

改造前的集成是一个隔离的 `plan_workflow` 工具：

- `code-agent/internal/runtime/flux_tool.go` 创建 Flux 自己的 `fluxmodel.OpenAICompatibleProvider`，没有完整复用 code-agent 当前 turn 的 credential、关联 ID、重试和计费汇总。
- Flux 内部只注册 `merge_result` 和 desktop shell，没有使用当前 workspace/session 的 code-agent、MCP 和 client tools。
- `code-agent/internal/tools/flux_adapter.go` 调用 Flux 时传入 `nil emitter`，执行期间对 code-agent 和客户端是黑盒。
- `plan_workflow` 在 base registry 阶段构造，无法看到随后注入的 workspace MCP 工具和 session-scoped client tools。
- iOS profile 当前禁用 `plan_workflow`，原因是 Flux 私有工具集依赖 shell，而不是 DAG 本身无法在 iOS 使用。
- Flux 的动态 WorkflowTool 当前使用自己的轻量 Scheduler；它只有简化节点状态，与 `flux-workflow/domain` 的 Task/Node 状态机、持久化、异步恢复和边闭合语义不一致。

这次集成的核心不是给 Agent 再增加一个黑盒工具，而是让 Flux 成为 code-agent 内部的确定性编排能力。

## 2. 已确认的架构决策

### 2.1 能力归属

| 能力 | 所属组件 |
|---|---|
| 对话、Turn、模型选择、Credential、计费 | code-agent |
| Agent 可见工具、权限、审批、hooks、MCP、client tools | code-agent |
| 目标到 DAG 的生成、校验和 repair | Flux |
| DAG 到 `WorkflowDefinition` 的编译 | Flux |
| Task/Node 状态机、依赖执行、异步等待、恢复、重试、持久化 | flux-workflow |
| DAG 和节点状态的客户端展示 | code-agent wire protocol + 客户端 |

Flux 不再发展第二套 filesystem、git、shell、web 等工具生态。Flux 自带工具仅用于独立示例、测试或最小运行，不作为嵌入 code-agent 时的能力来源。

### 2.2 唯一执行引擎

动态 DAG 校验成功后必须编译成 `flux-workflow` 的 `WorkflowDefinition`，创建不可变 WorkflowVersion 和 Task，并交给 `flux-workflow.Engine` 执行。

Flux 自己的轻量 Scheduler 不进入新的 code-agent 集成路径，并在迁移完成后废弃。不得先围绕轻量 Scheduler 建设工具、事件和 client-tool 适配层再迁移。

### 2.3 依赖方向

```text
code-agent
  ├── imports flux
  └── imports/adapts flux-workflow

flux
  └── imports flux-workflow

flux-workflow
  └── 不依赖 code-agent 或 Flux 上层 Runtime
```

Flux 不得导入 `code-agent/internal/model`。code-agent 在自身仓库内实现 Flux Provider 和 Tool Adapter。

暂不抽取 `github.com/tuxi/agent-provider`。Flux 独立使用时可以保留自己的 Provider；嵌入 code-agent 时必须注入 code-agent Provider adapter。

## 3. 目标架构

```text
Agent 调用 plan_workflow
        │
        ▼
Code-Agent FluxWorkflowAdapter
        ├── 当前 turn Provider / credential / correlation IDs
        ├── 当前 turn Tool Registry 投影
        ├── ToolExecutionHost / Approver / ClientWaiter
        └── UsageSink / WorkflowEventSink
        │
        ▼
Flux DAGPlanner
        │  generate → validate → repair
        ▼
WorkflowDefinition Compiler
        │
        ├── 持久化 Definition + immutable Version
        └── workflow_plan_ready
        │
        ▼
flux-workflow Engine
        ├── Task
        ├── NodeRuntime[]
        ├── AwaitBinding
        └── TaskEvent / State transitions
        │
        ▼
Code-Agent ToolExecutionHost
        ├── server tools
        ├── remote MCP tools
        └── iOS/macOS client tools
```

## 4. 执行流程

一次 `plan_workflow` 调用按以下顺序执行：

1. code-agent 为本次调用生成稳定 `workflow_id`，绑定 `parent_call_id`。
2. 使用当前 turn 的 code-agent Provider 调用 Flux DAGPlanner。
3. DAGPlanner 根据当前 turn 的工具目录生成 DAG，并执行 validate/repair。
4. 将校验通过的 DAG 编译为 `WorkflowDefinition`。
5. 持久化 WorkflowDefinition 和不可变 WorkflowVersion；记录 definition hash、tool catalog hash 和 planner 元数据。
6. 发出持久化的 `workflow_plan_ready` 事件，客户端此时即可绘制拓扑。
7. 创建 flux-workflow Task，使用 Engine 执行。
8. Engine 的 Task/Node 状态迁移通过事件桥接到 code-agent。
9. 每个工具节点通过 code-agent `ToolExecutionHost` 执行，不直接调用底层 `Tool.Execute`。
10. Task 结束后返回标准 `TaskOutput`、节点输出摘要、assets 和最终拓扑快照。

恢复、重试、fork 和局部重做必须使用已持久化的 WorkflowVersion，不得重新调用 LLM 生成 DAG。

## 5. Provider 与计费闭环

### 5.1 Provider Adapter

code-agent 内新增 `FluxCompleterAdapter`，把 Flux model 类型转换为 code-agent model 类型，并调用当前 turn 的 `model.Provider`。

Adapter 必须携带：

- `SessionID`
- `TurnID`
- `RequestID`
- `ExecutionID`
- 当前 ModelName
- 当前 session credential
- UsageSink

每一次 generate/repair 都是独立模型调用，必须分别上报 usage。不得只记录最后一次调用。

### 5.2 计费语义

- DAGPlanner 的 LLM usage 计入当前 turn 的 `ModelBillingUnits`。
- DAG 节点中的 managed tool receipt 计入 `ToolBillingUnits`。
- 本地工具不产生 BillingUnits。
- `BillingUnits = ModelBillingUnits + ToolBillingUnits`。
- 同一 `node_call_id` 的 managed tool receipt 必须幂等去重。

Planner usage 不得伪装为 `plan_workflow` 的普通 ToolUsage，否则会混淆模型费用和托管工具费用。

## 6. 工具注册和执行

### 6.1 动态 Registry 投影

Flux 工具目录必须在每次 `plan_workflow` 执行时从当前 turn registry 动态生成，而不是在 `BuildBaseRegistry` 阶段固定。

这样才能包含：

- 当前 workspace 的内置工具；
- workspace-scoped MCP tools；
- session-scoped client tools；
- 当前 profile 和配置允许的工具；
- 热重载后的 MCP/skill 相关能力。

### 6.2 默认排除项

以下控制面工具不得成为 DAG 节点：

- `plan_workflow`
- `task`
- `enter_plan_mode`
- `propose_plan`
- `ask_user`
- `todo`
- runtime internal tools

避免 Flux 递归调用自身、生成子 Agent 控制流或绕过 Runtime 约束。

### 6.3 ToolExecutionHost

实施为 `agent.Runner.ExecuteNestedTool()`（`internal/agent/nested_tool.go`）。Flux DAG 节点不直接调用 `Tool.Execute`，而是通过 `NestedExecutor.ExecuteNestedTool()` 走与 Agent Loop 相同的受控路径。该路径覆盖：

- ✅ side-effect 审批（`nested_tool.go:69`：`r.approve(tool.Name(), input)`）
- ✅ Inspector（`nested_tool.go:63`：`inspector.Inspect(input, r.WorkspaceRoot)`）
- ✅ pre/post hooks（`nested_tool.go:60,90`：`r.preHookBlock()` / `r.postHook()`）
- ✅ workspace/path boundary（Inspector 检查路径越界）
- ✅ client-tool dispatch（`nested_tool.go:73-86`：`r.ClientWaiter.Wait()`）
- ✅ 事件发射（`nested_tool.go:32,45`：emit ToolStarted + ToolFinished；stdout/stderr 通过 `ExecutionContext.OnStdout/OnStderr`）
- ✅ managed tool usage 和幂等计费（`nested_tool.go:52`：`result.Usage` 经 flux_tool.go `nestedUsageCollector` 汇总后返回 `NestedUsages`）

每个节点使用稳定的调用标识：

```text
node_call_id = stable(parent_call_id, workflow_id, task_id, node_name, attempt)
```

重放同一次 attempt 时保持不变；真正 retry 时 attempt 递增。`codeAgentFluxTool`（`flux_tool.go:378-382`）使用 `{parent_call_id}:{nodeName}:attempt-{n}` 生成 `nestedCallID` 传给 `ExecuteNestedTool`，Engine 的 retry 路径（`task_retry.go`）递增 `NodeRuntime.Attempt`，保证新 attempt 产生不同的 call_id。

## 7. Workflow、Task 和 Node 身份

| 标识 | 生命周期 |
|---|---|
| `parent_call_id` | Agent 对 `plan_workflow` 的一次 tool call |
| `workflow_id` | 从规划开始到最终结束的逻辑 Workflow Run |
| `workflow_definition_id` | 可编辑的工作流定义身份 |
| `workflow_version_id` | 一次规划生成的不可变定义版本 |
| `task_id` | flux-workflow 的一次实际执行/fork/retry |
| `root_task_id` | Task 树根身份 |
| `node_runtime_id` | 一次 Task 中一个 NodeRuntime 记录 |
| `node_call_id` | 节点某次工具执行的 wire/计费关联身份 |

`workflow_id` 在 LLM 规划前生成；规划阶段尚无 `task_id`。Task 创建后再绑定二者。

## 8. 状态协议

客户端状态必须以 `flux-workflow/domain/node_runtime.go` 和 `domain/task.go` 为事实源。code-agent 不重新创造近似的执行状态。

### 8.1 外层 WorkflowStage

LLM 规划发生在 Task 创建前，`planning` 不属于 flux-workflow TaskStatus，因此定义外层阶段：

```text
planning   # DAG 生成、校验、repair、编译和版本持久化
executing  # Task 已创建，由 Engine 执行或挂起
terminal   # Task 已进入终态，或规划阶段不可恢复地失败
```

### 8.2 TaskStatus

wire 值直接使用 domain 字符串：

| 状态 | 终态 | 含义 |
|---|---:|---|
| `pending` | 否 | Task 已创建，等待执行 |
| `running` | 否 | DAG 正在推进 |
| `suspended` | 否 | 等待 async/client/sub-workflow 外部完成事件 |
| `success` | 是 | 工作流成功 |
| `failed` | 是 | 工作流失败 |
| `canceled` | 是 | 工作流取消 |

允许迁移：

```text
pending   → running | canceled
running   → suspended | success | failed | canceled
suspended → running | failed | canceled
```

`suspended` 是 active 状态，不是终态。

### 8.3 NodeState

wire 值直接使用 domain 字符串：

| 状态 | 终态 | 客户端语义 |
|---|---:|---|
| `pending` | 否 | 等待依赖或调度 |
| `ready` | 否 | 依赖满足，正在解析和校验输入 |
| `running` | 否 | 工具正在执行 |
| `awaiting` | 否 | 等待异步回调/client tool/子工作流 |
| `retrying` | 否 | 执行失败，准备再次运行 |
| `success_pending_edges` | 否 | 工具成功，正在计算和持久化出边 |
| `failed_pending_edges` | 否 | 工具失败，正在关闭出边 |
| `success` | 是 | 节点成功且边已闭合 |
| `failed` | 是 | 节点失败且边已闭合 |
| `skipped` | 是 | 条件未命中或可选失败降级 |
| `canceled` | 是 | 节点取消 |

不得把 `success_pending_edges` 提前映射成 `success`，也不得把 `failed_pending_edges` 提前映射成最终 `failed`。

客户端可以使用服务端提供的派生展示字段，但不能代替原始状态：

```json
{
  "state": "success_pending_edges",
  "phase": "finalizing",
  "terminal": false,
  "successful": true
}
```

展示 phase 映射：

| NodeState | phase |
|---|---|
| `pending` | `queued` |
| `ready` | `preparing` |
| `running` | `executing` |
| `awaiting` | `waiting` |
| `retrying` | `retrying` |
| `success_pending_edges`, `failed_pending_edges` | `finalizing` |
| 四种终态 | `terminal` |

## 9. 事件协议与可观察性

### 9.1 原则

- 状态是事实，日志是描述；客户端不得通过日志或 started/finished 文案推断状态。
- 状态变化只由 snapshot 或规范的 `*_state_changed` 事件驱动。
- DAG 在执行前发送，客户端可以从第一节点开始前显示完整拓扑。
- 高频进度允许丢失，状态和最终输出必须可重放。

### 9.2 核心事件

```text
workflow_started
workflow_plan_ready
workflow_task_state_changed
workflow_node_state_changed
workflow_node_progress
workflow_node_log
workflow_node_output
workflow_finished
workflow_failed
```

状态事件示例：

```json
{
  "type": "workflow_node_state_changed",
  "workflow_id": "wf_123",
  "parent_call_id": "call_456",
  "task_id": 1001,
  "node_runtime_id": 2031,
  "node_name": "generate_video",
  "node_call_id": "nodecall_789",
  "from": "running",
  "to": "awaiting",
  "progress": 0.35,
  "sequence": 104
}
```

### 9.3 事件等级

沿用 flux-workflow `TaskEvent.EventGrade` 语义：

| Grade | 用途 |
|---|---|
| `transient` | 高频 progress、stdout/stderr；仅实时推送 |
| `persistent` | plan ready、状态迁移、最终输出；入库、排序、推送、replay |
| `audit` | 审批、人工 retry/fork、计费审计；入库但默认不推 UI |

### 9.4 Snapshot + Sequence

客户端初次连接或重连：

1. 拉取 Workflow snapshot；
2. snapshot 包含 `snapshot_sequence`、Task、完整拓扑和全部 NodeRuntime；
3. 订阅 `sequence > snapshot_sequence` 的持久化增量事件；
4. transient progress 丢失不影响正确状态。

Snapshot 至少包含：

- workflow identity 和 stage；
- WorkflowDefinition/Version identity；
- TaskStatus、Task progress、错误和最终输出；
- nodes、edges、activated edge 状态；
- 每个 NodeRuntime 的 state、progress、timestamps、error、output/assets、reuse/dirty metadata。

## 10. 拓扑和最终输出

`workflow_plan_ready` 在 DAG 校验和 WorkflowVersion 持久化后、Task 执行前发送：

```json
{
  "type": "workflow_plan_ready",
  "workflow_id": "wf_123",
  "workflow_version_id": 91,
  "goal": "生成项目分析报告",
  "nodes": [
    {"name": "scan", "tool": "list_files", "depends_on": []},
    {"name": "read", "tool": "read_file", "depends_on": ["scan"]}
  ],
  "edges": [
    {"from": "scan", "to": "read"}
  ]
}
```

最终结果以 flux-workflow `TaskOutput` 为核心：

- `result_type`
- `primary_file_url`
- `cover_url`
- `preview_url`
- `width` / `height` / `duration`
- `extras`

code-agent 额外返回：

- `workflow_id` / `task_id`；
- 最终 graph snapshot；
- 节点输出摘要；
- 标准化 assets；
- billing summary。

大输出不得完整塞入对话 observation；使用结构化 output、asset refs 和 reference ledger，模型侧只接收摘要与稳定 handle。

## 11. iOS/macOS Client Tools

client tools 不由 Flux 自己实现，而是当前 session registry 的一部分，通过动态工具投影进入 DAGPlanner。

执行语义：

```text
TaskRunning
NodeReady
NodeRunning
提交 client tool call
NodeAwaiting
TaskSuspended（若当前无其他可推进节点）
客户端返回 tool_result
TaskRunning
NodeSuccessPendingEdges / NodeFailedPendingEdges
NodeSuccess / NodeFailed
```

`ClientToolWaiter` 应适配为 flux-workflow async provider/AwaitBinding，而不是在 Flux 私有 Scheduler 中阻塞等待。

约束：

- Photos、HealthKit、AVFoundation 和需要 UI 权限的 client node 默认串行；
- server/MCP 节点可按 DAG 并行；
- client 断连、超时、取消必须落入 AwaitBinding、TaskStatus 和 NodeState；
- 回传结果必须关联 `node_call_id`，重复回传幂等；
- resume 后不重新规划 DAG。

## 12. 实施阶段

### 12.0 当前落地状态（2026-07-23）

- Flux 的新执行路径已改为把动态 DAG 编译成 `WorkflowDefinition`，交由 `flux-workflow Runtime/Engine` 执行；旧轻量 Scheduler 不再用于该路径。
- code-agent 已接管 Flux 规划所用 Provider、请求身份透传、模型用量汇总、当前 turn 工具投影、审批/hooks/client dispatch 和 managed-tool 用量汇总。
- flux-workflow 已补充规范的 Task/Node 状态迁移事件；code-agent 已桥接 plan、拓扑、状态和终态输出到 wire event。
- `plan_workflow` 已可在 iOS profile 注册，且 iOS arm64 交叉编译通过。
- client tool 节点已编译为 Engine 原生 Await 节点：先持久化 AwaitBinding 并进入 `awaiting/suspended`，再通过 session-scoped client waiter dispatch；客户端回传后由 Engine 完成节点、恢复 Task 并继续下游。普通 WebSocket 断线重连沿用同一 session waiter，不丢失等待调用。
- Phase 0 golden fixtures 已补齐 21 个 workflow golden 文件，覆盖全部事件类型及关键状态迁移。
- Phase 1 执行底座收敛：旧 Scheduler/Engine/Plan 已标记 Deprecated；Retry 已通过 `plan_workflow(action=retry)` 暴露（从 `.codeagent/flux-workflows/{id}.db` 恢复 Runtime，重新注册工具，Start worker，bridge 事件，Retry 入队并等待终态）。
- Phase 2 ToolExecutionHost 确认已存在：实施为 `agent.Runner.ExecuteNestedTool()`（`nested_tool.go`），审批/Inspector/hooks/client dispatch/事件/usage 全在同一受控路径。Flux DAG 节点通过 `codeAgentFluxTool` → `NestedExecutor.ExecuteNestedTool()` 不绕过 Runtime。
- §16 设计细节已落定 6/7 项，仅剩 TaskOutput 中 URL/assets 如何转换为 code-agent GatewayAssetRef。
- 进程重启后的自动 rehydrate、重新投递尚未实施；当前恢复范围是进程存活期间的客户端断线重连。
- Phase 4 snapshot API 已实现（`GET /v1/conversations/{id}/workflow/{workflow_id}/snapshot`）——从 `.codeagent/flux-workflows/{id}.db` 直接聚合 Task/NodeRuntime/拓扑/snapshot_sequence，客户端一次请求拿到全貌。

### Phase 0：冻结协议与兼容测试

- 确认本文的身份、状态、事件和 snapshot 契约；
- 为 NodeState/TaskStatus 映射编写表驱动测试；
- 为 wire JSON 建 golden fixtures；
- 定义未知新增状态的客户端兼容策略；
- 明确 WorkflowDefinition 持久化和版本不可变规则。

验收：客户端可以只基于协议开发 mock DAG 页面，不依赖运行实现。

### Phase 1：Flux 执行底座收敛

- DAGPlanner 输出校验后的 DAG ✅（2026-07-23 端到端验证通过）
- 编译为 flux-workflow WorkflowDefinition ✅（`spec_to_workflow.go`）
- 持久化 Definition + immutable Version ✅（`Runtime.RegisterWorkflow`）
- 创建 Task，由 flux-workflow Engine 执行 ✅（code-agent `FluxWorkflowTool` 直接使用 `Runtime.Run`）
- `WorkflowTool` 返回 TaskOutput ✅
- 删除或退役 Flux 轻量 Scheduler ✅（2026-07-23：`Engine`、`Scheduler`、`Plan`、`MemState` 已标记 Deprecated；保留仅用于存量消费者 dream-ai。code-agent 路径不经过旧 Scheduler）
- 确认 sync、failure、retry、await、resume、cancel、edge closure 行为 ✅（2026-07-23：await/resume/cancel/edge closure 已验证；retry 已通过 `plan_workflow(action=retry)` 暴露——恢复 Runtime → 注册工具 → Start workers → bridge events → Retry → 等待终态）

验收：所有动态 DAG 都能从 Task/Node repository 得到真实状态，恢复时不重新规划。

### Phase 2：Code-Agent 闭环

- 实现 FluxCompleterAdapter ✅（`flux_tool.go`：复用当前 turn 的 `model.Provider`，透传 SessionID/TurnID/RequestID，usage 计入 turn）
- 规划模型 usage 汇入 turn ✅（`fluxUsageCollector` 收集每次 LLM 调用的 usage，通过 `ModelUsage` 字段返回）
- 实现当前 turn registry 动态投影 ✅（`projectFluxTools`：每次 `Execute` 时从 `ec.ToolRegistry` 动态生成，排除 `plan_workflow`/`task` 等控制面工具）
- 提炼 ToolExecutionHost ✅（实施为 `agent.Runner.ExecuteNestedTool()`，覆盖审批/Inspector/hooks/client dispatch/事件/usage。见 §6.3 更新后的描述）
- 连接 server tools 和 remote MCP tools ✅（通过 `projectFluxTools` → `codeAgentFluxTool` → `NestedExecutor.ExecuteNestedTool` → `r.executeTool()` 统一路径）
- 建立 WorkflowEvent bridge 和 snapshot query ⚠️（事件桥接已做：`bridgeFluxEvents` 订阅 flux-workflow Engine 事件并转为 `agent.Event`；snapshot query 属于 Phase 4）
- desktop 端到端测试 ✅（Python 5 步 DAG 项目搭建全链路验证通过）

验收：Agent 可调用真实 code-agent/MCP 工具完成 DAG，权限、事件和计费均不绕过 Runtime。

### Phase 3：Client Tools 与 iOS

- client proxy tools 投影给 DAGPlanner；
- ClientToolWaiter/AwaitBinding adapter；
- 断连、超时、取消、重复回传；
- iOS sandbox profile 启用 `plan_workflow`；
- client/server 混合 DAG 测试。

验收：DAG 可以暂停等待 iOS 工具、回传后恢复，并将结果传给下游节点。

### Phase 4：客户端 DAG UI

- 拓扑布局和节点状态着色 ✅（客户端 DAG 组件已实现）
- 节点进度、日志、输入、输出和 assets ⚠️（客户端侧实现中）
- Task suspended/resume/cancel ⚠️（cancel 服务端 Engine 已有，待 wire 暴露）
- 历史 snapshot/replay ✅（2026-07-23：`GET /v1/conversations/{id}/workflow/{workflow_id}/snapshot` 端点已实现——返回 Task 状态 + 全部 NodeRuntime + 拓扑 edges + snapshot_sequence。客户端流程：一次 pull 拿全部当前状态 → 订阅 `seq > snapshot_sequence` 增量事件）
- 失败节点 retry、fork 和局部重做 ✅（retry 已通过 `plan_workflow(action=retry)` 暴露；fork 服务端 Engine 已有，待 wire）

验收：断线重连后 UI 与 repository 状态一致，不依赖丢失的 transient events。

## 13. 迁移策略

1. 先在 Flux 内完成 Engine 收敛，不为旧 Scheduler 增加新的 code-agent adapter。
2. 短期可保留旧执行路径作为测试对照，但 code-agent 新集成只使用 Engine backend。
3. 用相同 DAG 对比旧/新路径的节点输出、错误传播和完成状态。
4. Engine 路径通过 sync/async/resume/cancel 回归后移除旧路径。
5. code-agent 的 `plan_workflow` 从 base-time 固定实例改为 turn-scoped 动态组装。
6. iOS feature gate 在 client-tool await adapter 完成后打开。

## 14. 测试矩阵

最低必须覆盖：

| 场景 | 核心断言 |
|---|---|
| 线性 DAG | 拓扑、状态顺序和输出映射正确 |
| 并行 DAG | 独立节点并行，join 等待全部要求的父节点 |
| 条件分支 | 未激活分支 skipped，边状态闭合 |
| 输入校验失败 | ready → failed，Task 最终 failed |
| 复杂字面量参数 | 多行字符串、引号、反斜杠、数组和嵌套对象经 InputMapping 后保持原值 |
| 工具失败重试 | running → retrying → running，attempt/call ID 正确 |
| 可选节点失败 | 节点 skipped，不拖垮 Task |
| async provider | running → awaiting，Task suspended，回调后 resume |
| client tool | 客户端执行、断连、超时、重复 result 幂等 |
| MCP tool | workspace-scoped registry 生效 |
| side effect | approval/Inspector/hook 不被绕过 |
| planner repair | 每次模型调用 usage 独立累计 |
| cancel | Task/Node 状态进入 canceled，等待被解除 |
| crash recovery | 使用原 WorkflowVersion 恢复，不重新规划 |
| reconnect | snapshot + sequence 得到一致 UI |
| managed billing | node_call_id 去重，总 BillingUnits 正确 |

## 15. 非目标

本版本不做：

- 抽取公共 `agent-provider` module；
- 让 Flux 自建一套与 code-agent 重复的工具；
- 让 Flux 管理 Conversation/Turn；
- 在第一阶段完成完整 DAG 可视化 UI；
- 允许 DAG 节点递归调用 `plan_workflow` 或 `task`；
- 用日志文案代替结构化状态协议。

## 16. 实施前仍需落定的细节

以下项目不改变总体架构。已落定的标 ✅；仍待 spike 的保持编号。

1. ✅ **Generated WorkflowDefinition 存储**：每个 workflow 一个独立的 SQLite 文件，路径 `<workspace>/.codeagent/flux-workflows/<workflow_id>.db`，由 `flux-workflow.Runtime.NewLocal(dbPath)` 管理。不进入 Flux 自己的 repo；code-agent 不提供额外 Store adapter。（2026-07-23 落定，`flux_tool.go:194-198`）
2. ✅ **`workflow_id` 格式**：`”wf_” + hex.EncodeToString(SHA256(SessionID + “\x00” + TurnID + “\x00” + CallID + “\x00” + goal)[:8])`。确定性哈希，同一次 `call_id + goal` 重入幂等。规划阶段无 `task_id`，`task_id` 在 `Runtime.Run()` 创建 Task 后分配。（2026-07-23 落定，`flux_tool.go:180-182`）
3. ✅ **Node attempt 持久化位置**：`flux-workflow/domain.NodeRuntime.Attempt` 字段，由 Engine retry 路径（`task_retry.go`）在每次重试时递增。`node_call_id` 公式 `stable(parent_call_id, workflow_id, task_id, node_name, attempt)` 在 managed tool billing（Phase 2 ToolExecutionHost）时计算，code-agent 侧暂不需要独立维护。（2026-07-23 落定）
4. ✅ **Workflow events 分区策略**：所有 workflow event 发送到父 conversation 的 event stream，通过 `workflow` payload 中的 `workflow_id` 字段区隔。暂不创建独立的 `/v1/workflows/{id}/events` 端点。job 的双写模型（父 partition + 独立 partition）不适用于 workflow。（2026-07-23 落定）
5. ToolExecutionHost 的包边界，避免 `internal/agent` 与 `internal/tools` 形成循环依赖。
6. TaskOutput 中 URL/assets 如何转换为 code-agent GatewayAssetRef 和本地 assetref.Ref。
7. ✅ **client-tool waiting 时的 suspended 时机**：以 `flux-workflow Engine` 最终 `TaskStatus` 为准。Engine 在所有非 awaiting 节点都完成且至少有一个节点在 awaiting 时进入 suspended。客户端不得自行推断。（2026-07-23 落定，实现已遵循此规则）

剩余 5、6 两项应通过小型 spike 和契约测试解决，不能通过客户端猜测补齐。
