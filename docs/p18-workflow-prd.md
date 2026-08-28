# P18 — Workflow 面板与调度组合 需求文档（PRD）

状态：草案 v0.2（已按评审修订，待复评）
日期：2026-08-26
评审：独立 session 20260826-102701-4164d518（REQUEST_CHANGES → 已修订 B1–B7/C1–C4）
关联：p15-scheduled-tasks-prd、p16-automation-task-breakdown、p17-automation-frontend-requirements、runtime-integration/flux-dynamic-dag-integration-v1

---

## 1. 定位

一句话：**对话是 IDE，workflow 是构建产物，automation 是 CI 触发器，App 面板是 CI dashboard。**

Agent 已有三种隐含执行模式：

| 模式 | 反思 | 适用 |
|---|---|---|
| 直接做 | 有 | 单步 |
| plan mode → 审批 → 回合内执行 | 有 | 边做边判断 |
| plan_workflow → 冻结 DAG | 无 | 步骤已知，要确定性 |

Workflow 的正确定位是**计划收敛之后的执行形态**。生命周期是一条链：

```
对话探索（发散执行） → 固化（两种入口） → automation 定时触发 → 面板观测/恢复
                        ├─ 模型规划：直接产出 manifest（现有 plan_workflow）
                        └─ 轨迹提取：把一次成功的对话执行编译为 manifest（R9）
```

核心价值：**探索用发散，生产用收敛**。让模型自由发挥找出最优路径，再把它冻结成确定性资产，消除「每次规划都有差异」带来的不确定性。

## 2. 非目标（Non-goals）

- **不做可视化 DAG 编辑器**。节点连线 UX、参数映射、错误路径设计成本极高，且用户身边有能一句话编译 manifest 的模型，让人手画图方向反了。（Coze/n8n/OpenAI Agent Builder 属于此阵营，明确不进。）
- **App 不暴露节点级编辑**。自由 DAG 创作只发生在对话里，由模型产出 manifest，人通过审批门确认。
- **不引入自由 DAG 创作**；模板类型仅两类：既有 `cross_workspace_collaboration_v1/v2`（跨会话派发，保留），新增 **`tool_sequence` 通用工具序列模板**（R9 专用：单会话内工具调用链，一个工具调用 = 一个节点）。后者是 R9 落地的必要条件（评审 B1 裁定引入）。

## 3. 现状盘点（已核实）

### 3.1 已有的存储（flux-workflows.db，per-workspace）

路径：`<workspace>/.codeagent/flux-workflows/flux-workflows.db`

| 表 | 内容 |
|---|---|
| `workflows` | id / user_id / name / description —— 命名模板实体 |
| `workflow_versions` | workflow_id / version / definition_json / hash —— 不可变版本 |
| `tasks` / `task_nodes` / `task_events` | 运行状态机 |
| `await_bindings` | 客户端工具挂起等待 |
| `task_cost_traces` | 成本追踪 |

**关键事实：plan_workflow 每次运行已经在写这两张表**——`flux_tool.go:265` 调 `RegisterWorkflow` 落定义，`:226` 调 `Submit(def.Name)` 起 Task。面板需要的数据每次运行都在产生，只是没有暴露。

### 3.2 已有的引擎 API（github.com/tuxi/flux-workflow v1.0.6）

- `repository.WorkflowRepository`：Create / Update / GetByID / GetByName / List（**无 Delete**）
- `repository.WorkflowVersionRepository`：Create / Get / GetLatestByWorkflowID / GetLatestByWorkflowName / UpdateDefinitionJSON
- `runtime.Runtime`：RegisterWorkflow(def)、Submit(workflowName, input)、Status(taskID)、Retry(taskID, resumeFrom)、Resume、CompleteAwait、DB()（gorm 直连）、EventBus()、Subscribe()
- **引擎失败模型（已核实）**：任务启动构建阶段（workflow/builder.go 逐节点 `factory.Create`）校验全部定义——缺工具报 `tool not found: X`（tool_factory.go:29-32）、边非法/成环报错（builder 环检测 + validateEdges）；表达式求值基于 expr-lang（nodes/context.go）——引用不存在节点名在 `expr.Compile` 即报错，引用已存在节点的缺失字段求值得 nil（配合 `?? ''` 兜底）。即：**定义类错误全部 fail-fast，不产生中途静默失败**。但 RegisterWorkflow 只持久化不校验，缺工具模板能保存、每次触发才报错。

### 3.3 已有的服务端能力

- 快照：`GET /v1/conversations/{id}/workflow/{workflow_id}/snapshot`（mux.go:1502），一次请求聚合 Task/NodeRuntime/拓扑/snapshot_sequence
- 事件桥：`workflow_*` 事件族已桥接到 agent.Event，经 WS 推送
- 重试：plan_workflow `action=retry` + resume_from，从持久化 DB 恢复 Runtime
- 审批门：`WorkflowPlanApproval`（flux_tool.go:202）——编译产物出厂前的人工确认
- 自动化：`internal/automation`（store/scheduler/TurnDispatcher，chat/reuse/standalone 三模式）、`/v1/automations` CRUD、对话内 `automation` 工具、App 面板；权限上下文 Perm（permission_mode/connectors/skills）

### 3.4 缺口

1. 无 `/v1/workflows` 类端点——客户端无法列出/查看/触发工作流（App 做不了面板的根因）
2. 触发路径长在会话回合里（flux_tool.go Execute 流程内：工具投影注册、事件桥绑定会话、ExternalResolver），不能脱离回合调用
3. automation 只能投 prompt turn，不能直接拉起 workflow run；无重叠策略（overlap policy）
4. 无「保存为命名模板」动作——每次运行的 definition 名字由 goal+agents 哈希派生，是一次性身份而非可复用资产
5. 无「从对话轨迹提取工作流」能力——自由对话中跑顺的工具调用序列无法固化为可复用模板（R9）
6. 无「无回合 ExecutionContext」工厂——headless 触发没有 turn 派生的 ToolRegistry/NestedExecutor/身份，需新造（评审 B7）

## 4. 需求列表

优先级：P0=零成本立即可做；P1=只读数据面；P2=模板化与触发；P3=自动化组合与恢复；P4=轨迹提取（独立一期）。
来源标注 = 该设计借鉴自哪个项目。

### R1 工作流目录与详情（P1｜来源：GitHub Actions）

- 列出 workspace 内全部工作流：名称、描述、最新版本 hash、最近一次 run 的状态/进度/错误
- 详情页：定义（nodes/edges/output）、版本历史、run 历史
- 删除（软删；repository 接口无 Delete，需 gorm 直连补齐）
- 验收：App 面板不依赖任何会话即可渲染目录

### R2 运行观测（P1｜来源：GitHub Actions）

- 会话内 run：复用 `GET /v1/conversations/{id}/workflow/{wid}/snapshot`；**headless run（R5/R6 触发）新增 workspace-scoped 观测面**（`GET /v1/workspaces/{path}/workflows/{name}/runs/{task_id}/snapshot`），两者共享与会话无关的底层 `NewWorkflowSnapshotFunc`（评审 B2）
- WS 订阅 `workflow_*` 事件实时更新
- 运行详情页布局对齐 GH Actions：任务侧栏 + 日志区 + 操作菜单（含 **Cancel**——引擎需补 Cancel API，见 Q12）
- 验收：从面板打开会话内与 headless 两类 run，均无需进会话即可看到全貌

### R3 失败恢复（P2｜来源：GitHub Actions）

- 「Re-run failed」语义：只重跑失败节点及其下游；重跑前列出受影响的下游节点并要求确认
- **不是薄端点**：retry 是完整 seam（newFluxRuntime + 工具投影 + 事件桥 + 阻塞等待终态），面板端点改为**异步发起**（202 + 立即返回，进度走事件流），避免按 run 时长阻塞的 HTTP 长轮询（评审 B3）
- **重试粒度定义**：`tool_sequence` 模板 = 节点级（`resume_from` 节点名直接对应）；v1/v2 Map 拓扑 = 仅支持「整 run 重跑」+「按任务级节点名恢复」，item 级部分重跑不在本期（评审 B3）
- 验收：失败 run 可从面板异步发起部分重跑，确认框正确枚举下游（tool_sequence 拓扑）

### R4 模板化（P2｜来源：GitHub Actions reusable workflows）

**模板数据模型（已定）**——不引入新格式，不建新表：

```
workflows 表（命名实体）: name / description / is_template / source / manifest_json(新列)
  ├─ workflow_versions.definition_json   ← 编译后 WorkflowDefinition（引擎执行事实源，hash 版本化）
  └─ workflows.manifest_json             ← 源 manifest（goal/template/agents[]/parallelism，code-agent 业务事实源）
```

- **格式**：definition_json（声明式 JSON）= 执行事实源；manifest_json = 编辑/表单生成事实源。**不用 md/yaml 作存储**——yaml 仅可选作「人写模板」的输入格式，md 仅作描述文档（workflows.description 已覆盖）。
- **「保存为模板」**：从 run 的 `task.input_json` 恢复源 manifest → 用户命名 → `RegisterWorkflow(def)`（落定义+版本）+ 持久化 manifest。零新表。
- **版本化**：复用 `workflow_versions` hash 机制——同 hash 跳过、JSON 变更（非语义）UpdateDefinitionJSON、hash 变更发新版本。
- **对话修改模板**（R8 workflow 工具 `edit` 模式）：读最新 definition + manifest → LLM 修改 manifest → dry-run compile 校验 → 审批门 → `RegisterWorkflow` 自动发新版本，旧版本保留。
- **目录区分**：`is_template`/`source` 标记区分「可复用模板」与「一次性 run 痕迹」（评审 C1）。
- 与 R9 的 `tool_sequence` 模板共用同一存储（workflows/workflow_versions），仅 definition 来源不同。
- 验收：任意完成的 run 可保存为命名模板；模板可对话修改出新版本；可被触发

### R5 参数化触发（P2｜来源：Windmill）

- 运行表单**从 manifest 声明字段自动生成**——v1/v2 模板固定 schema：`goal` + `template` + `agents[]`（角色/会话/消息/验收标准的花名册表单）+ `parallelism` + `timeout_ms`，不为每个模板手写表单
- **v1 不做内嵌变量槽位**（如消息里的 `{{date}}`）；manifest_json 预留 `input_schema` 字段（Windmill 式声明式表单 schema），作为 v2 增强，届时从该字段渲染表单
- 每个模板一个触发端点（webhook 思路）：`POST /v1/workflows/{name}/runs?workspace=<abs_path>`
- 触发走「headless run seam」（见 §6）：注册定义 → 投影工具入 ToolRegistry → 接事件桥与 ExternalResolver → Submit，全程脱离会话回合
- 触发端点鉴权：继承 workspace 权限 + 防滥用（节流/配额），模型待定（评审遗漏项 2 → Q13）
- 验收：在 App 表单填参数直接起一个 run，进度实时可见

### R6 自动化组合（P3｜来源：Temporal Schedules）

**数据模型（automations 表新增，已定）**：

```sql
workflow_ref    TEXT  -- "workspace_path#workflow_name"，空 = 维持 prompt turn
workflow_input  TEXT  -- 触发参数 JSON（如 {"instId":"BTC-USDT-SWAP"}），每次触发传给模板
overlap_policy  TEXT  -- skip | buffer_one | buffer_all | allow_all，默认 skip
```

**Dispatcher workflow 分支**（workflow_ref 非空时）：
1. **Overlap 判定**：查 flux-workflows.db `tasks` 表该 workflow 最新版本的活跃 run（pending/running/suspended）——有活跃 run 时按 overlap_policy 处理（评审 B5：不能复用调度器 Running 标志，dispatch 后即清除）
   - skip：跳过本次触发，记 run 记录（status=skipped）
   - buffer_one/buffer_all：v2 再做（需队列表，Q15）；v1 仅实现 skip + allow_all
   - allow_all：无条件触发
2. **触发**：`SubmitHeadlessRun(workspace, name, workflow_input)`（R5 seam，含工具投影）
3. **失败策略**：run 中途失败 ≠ 投递失败，MaxRetries 不适用；**默认跳过等下次调度**（评审 B6）。run 失败详情在 workflow 面板可见（用户可手动 resume）
4. **返回值语义**：Dispatch 返回的标识写入 automation_runs.task_id（新列），**不写入 Run.SessionID / reuse 会话 id**（评审 B6）
5. **Pause**：复用既有 status=PAUSED（评审 B4）；Trigger now = 立即 dispatch 一次

**automation 工具扩展**：create/update 支持 workflow 模式——`workflow_ref` + `workflow_input` + `overlap_policy` 三个新字段；prompt 与 workflow_ref 互斥（二选一）

**App Automation 面板**：创建/编辑表单支持 workflow 模式——选模板（workflow 列表选择器）+ 填触发参数（从模板 manifest 生成表单，复用 Workflow 触发表单的 manifest 预填逻辑）+ overlap policy 选择

**验收**：一条 automation 绑定模板按 rrule 定时触发（无人值守、0 LLM token），重叠行为符合所选策略，run 失败跳过等下次调度，面板可 Pause/Trigger now

### R7 挂起与恢复（P2/P3｜来源：LangGraph）

- 挂起事件载荷携带完整恢复上下文：等待什么、谁有权恢复、恢复需要什么参数（对齐 LangGraph interrupt payload 设计）
- App 渲染「恢复卡片」而非仅显示“已暂停”；恢复动作映射到 CompleteAwait/Resume
- 验收：含 client-tool 等待节点的 run 在面板中可完成人机交互恢复

### R8 会话内管理工具（P2）

- 对话内新增 `workflow` 工具，模式化设计镜像 `automation` 工具：save / list / view / delete / run / **extract**（R9 轨迹提取入口，评审 C2）
- 创建仍留在对话：模型产出 manifest → 审批门 → 存为命名模板
- 验收：“把这个流程存成模板，以后每天早上八点跑”一句话可在单个会话内完成 R4+R6 的串联；「把这个流程存成模板」单独即可触发 extract → 审批 → 落库

### R9 对话轨迹提取为工作流（P4｜来源：概念类比 Apple Shortcuts / RPA 录制，实现走 LLM 自编译）

把一次执行顺畅的自由对话（如「刷推特 → 互动 → 找今日热点 → 发推」）编译成固定工作流，用户一句「把这个流程存成模板」即固化。

- **实现方式：不做事件级程序合成**（从 tool 事件日志反推 DAG 等价于程序合成，成本极高）；改为 **LLM 自编译**——把本次对话的工具调用历史 + 结果喂给模型，让它用自己的执行记忆产出 `tool_sequence` 模板 manifest（节点 = 工具调用 + 入参模板 + 输出映射），走现有审批门后落 `workflows` + `workflow_versions`
- **前置依赖**：新增 `tool_sequence` 通用工具序列模板类型 + trace→WorkflowDefinition 编译器（评审 B1 裁定：引入）；节点参数绑定/输出映射语法定义见 Q16
- **定义校验（dry-run compile，已定）**：保存/更新模板前，用目标 workspace 的工具注册表构建 NodeRegistry + `workflow.Compile` 预编译 manifest，在保存时即暴露三类错误——① 工具不存在（`tool not found: X`）② InputMapping/Output 表达式引用未知节点（expr.Compile 报错）③ 边非法/成环（builder 校验）——把运行时失败提前为保存时失败。运行期保持 fail-fast 兜底：工具在运行 workspace 不可用时 run 启动即报错，不产生中途静默失败（引擎已核实行为）
- **参数化**：模型显式区分固定输入与变量输入（如「今天的热点」按日期变化，标记为每次运行需提供的参数）；v1 规则：来自用户输入或环境（当前日期/workspace）的部分标记为变量，其余视为固定
- **错误处理**：成功轨迹不含失败路径；v1 每个节点默认走引擎既有 retry 策略，模型编译时可标注「失败跳过或终止」
- **节点粒度**：v1 一个工具调用 = 一个节点，简单可预测；模型可按语义合并相关调用，但默认一对一
- **成功判定**：由用户显式发起（说「存成模板」或面板点按钮），不做自动嗅探
- 验收：对话中完成一次多步任务后，一句话生成可触发、可挂 automation 的命名模板，参数化字段正确标注

## 5. 交互原则

1. **创建在对话，管理在面板**。写 DAG 是模型最擅长的；App 只做列表/详情/触发/重试/恢复。
2. **实例化 ≠ 创作**。拓扑固定（v1/v2 模板），App 的“创建”退化为花名册式表单：选工作区、列 worker 角色/会话、写任务书和验收标准、设调度——这是排班，不是画图。
3. **accept/refuse 交互**（Kestra）：模型产出的定义以 diff/预览形式呈现，用户接受或拒绝，一切保持为一个声明式 artifact（definition_json 版本链）。
4. **定义是数据，UI 是观测器**（GitHub Actions/Windmill/Temporal 共同范式）：所有面板操作都落在「读快照、触发、重试、暂停、取消」五类原语上。
5. **先跑顺，再固化**（R9）：固化入口越轻（一句话/一个按钮），用户越愿意把成功的探索冻结成确定性资产。App 面板在 run 完成页提供「保存为模板」入口，对话里提供同义工具模式。

## 6. 接口草案

```
GET    /v1/workflows?workspace=<abs_path>                          # R1 目录（含 latest run 摘要）
GET    /v1/workflows/{name}?workspace=<abs_path>                   # R1 详情（定义+版本+runs）
POST   /v1/workflows/{name}/runs?workspace=<abs_path>              # R5 触发（headless run seam，鉴权见 Q13，shape 待 P2 确认）
GET    /v1/workflows/{name}/runs/{task_id}/snapshot?workspace=<abs_path>   # R2 headless 观测面
POST   /v1/workflows/{name}/runs/{task_id}/retry?workspace=<abs_path>      # R3 异步发起（202，shape 待 P2 确认）
POST   /v1/workflows/{name}/runs/{task_id}/cancel?workspace=<abs_path>     # R2/R6 取消（引擎需补 Cancel API，Q12，shape 待 P2 确认）
POST   /v1/workflows/{name}/template?workspace=<abs_path>          # R4 从指定 run 保存为模板（shape 待 P2 确认）
PATCH  /v1/workflows/{name}/template?workspace=<abs_path>          # R4 模板新版本（UpdateDefinitionJSON，shape 待 P2 确认）
GET    /v1/conversations/{id}/workflow/{wid}/snapshot              # 已有，会话内 run 复用
POST   /v1/conversations/{id}/workflow/{wid}/retry                 # 已有 retry 语义，面板入口包装
POST   /v1/conversations/{id}/workflows/extract                    # R9 对话轨迹→模板（主要入口在会话内 workflow 工具 extract 模式）
```

**路由形状说明（2026-08-26 修订）**：workspace 路径用 query 参数而非 URL 通配符，因为 Go 1.22 ServeMux 的单段 `{path}` 通配符与既有 `GET /v1/workspaces/permissions/{path...}` 必然冲突（`/v1/workspaces/permissions/workflows` 两条都可匹配且互不更具体，注册时 panic）。`/v1/workflows` 前缀无冲突。App 端 URL 形如 `/v1/workflows?workspace=/abs/path`，workspace 值需 URL 编码。

automation 表新增（R6）：

```
workflow_ref      TEXT    -- "workspace_path#workflow_name"，空 = 维持 prompt turn
overlap_policy    TEXT    -- skip | buffer_one | buffer_all | allow_all，默认 skip（判定落任务表，评审 B5）
-- Pause/Resume 复用既有 status=PAUSED，不新增字段（评审 B4）
```

核心工程量分为两块（评审 B7 拆分）：

1. **headless ExecutionContext 工厂**（P2 地基）：flux_tool.go Execute 强依赖 turn 派生的 ec（ToolRegistry/NestedExecutor/CallID/OnWorkflowEvent/WorkflowPlanApproval）。headless 场景需新造「无回合 ExecutionContext」——ToolRegistry 从 automation Perm.Connectors/Skills + workspace 配置构建、workflowID 身份重新定义哈希输入、事件桥落独立 workflow 事件流。此为本期**单独立项**的模块，不是抽 5 步函数。

**headless 注册表组成（tool_sequence 节点工具资格，已定）**：headless 注册表 = workspace 原子工具（文件/搜索/代码/系统：read_file、grep、run_command、web_search、list_files、find_symbol、recall_memory、create_file、edit_file、git_diff、download_file、extract_archive、create_pdf、read_pdf 等）+ 该 workspace 已配置的 MCP 工具（mcp__*）。**排除四类**：对话交互（ask_user/propose_plan/enter_plan_mode/todo_write）、工作流元管理（plan_workflow/workflow_status/workflow_list/workflow_definition/workflow_events）、子代理（task/send_to_session/wait_sessions/read_session——属于 v1/v2 跨会话模板领域）、自动化管理（automation/get_current_time）。**不引入额外 allowlist**——dry-run compile 用的就是 headless 注册表，不在其中的工具按 `tool not found` 拒绝保存。
2. **run seam**：在工厂之上，把「getOrCreateRuntime → projectFluxTools → registerFluxWorkflow → bridgeFluxEventsAsync → Submit」抽成包内函数，服务端触发与会话内 plan_workflow 共用。

R9 另需一个 **trace → manifest 编译**工具模式（`workflow` 工具的 `extract` mode）+ `tool_sequence` 模板编译器：读取当前会话的工具调用/结果事件，交给模型产出 manifest（节点 = 工具调用 + 入参模板 + 输出映射），返回给审批门。节点参数绑定/输出映射语法与校验见 Q16。

## 7. 分期

| 期 | 内容 | 交付判据 |
|---|---|---|
| P0 | skills/ 内沉淀「automation + plan_workflow」组合用法模式 | 一条 automation 能间接驱动 workflow（现状已通，只补文档） |
| P1 | R1+R2：只读端点（含 headless 观测面）+ App 目录/详情页 | 面板可见全部历史 run 与节点状态（会话内 + headless 两类） |
| P2 | headless ExecutionContext 工厂 + R3+R4+R5+R8：run seam、异步重试、模板化与更新、参数化触发、workflow 工具 | App 内完成「保存→填参→触发→观测→重试」闭环 |
| P3 | R6+R7：automation 绑定 workflow + overlap policy + 恢复卡片 | 定时无人值守跑模板，重叠可控，挂起可人机交互恢复 |
| P4 | R9：tool_sequence 模板类型 + trace→manifest 编译器 + extract 模式 | 对话内一句话把成功执行固化为可触发模板（依赖 P2 的 seam 与模板存储） |

每期独立可用、独立验收。

## 8. 借鉴对照表

| 项目 | 抄什么 | 对应需求 | 来源 |
|---|---|---|---|
| GitHub Actions | run 页布局、Re-run failed jobs + 下游确认、reusable workflows、matrix=fan-out | R1/R2/R3/R4 | [partial re-runs](https://github.blog/news-insights/product-news/save-time-partial-re-runs-github-actions)、[reusing workflow configurations](https://docs.github.com/en/actions/reference/workflows-and-actions/reusing-workflow-configurations) |
| Temporal Schedules | Schedule=spec+action+policy；Overlap Policy 四档；pause/trigger/backfill | R6 | [docs.temporal.io/schedule](https://docs.temporal.io/schedule) |
| Windmill | 表单从参数 JSON Schema 自动生成；每个 flow 自带触发端点 | R5 | [flow editor components](https://www.windmill.dev/docs/flows/editor_components) |
| Kestra | AI Copilot 生成声明式 YAML + accept/refuse；一切保持为代码 | §5 原则、R8 | [release 1.0](https://kestra.io/blogs/release-1-0)、[AI Copilot](https://kestra.io/docs/ai-tools/ai-copilot) |
| LangGraph | interrupt() 载荷设计：为什么停、怎么答；checkpoint 恢复 | R7 | [interrupt reference](https://reference.langchain.com/python/langgraph/types/interrupt) |
| Apple Shortcuts / RPA 录制（概念类比） | 录制成功操作序列再泛化为可复用流程；本需求以 LLM 自编译实现，不做事件级程序合成 | R9 | — |

共同点：没有一个项目要求用户画 DAG。定义是数据/代码，LLM 或开发者写它，UI 负责观测、触发、恢复。

## 9. 开放问题（评审时定）

- **Q1 run seam 边界**：事件桥目前绑定会话（ec.OnWorkflowEvent）；headless 场景事件落到哪——jobs 流？新 workflow 事件流？
- **Q2 无人值守审批**：定时触发的 run 过不过 WorkflowPlanApproval？建议：模板首次保存时人工审过，后续触发凭 permission_mode 放行，审批门只在「未审过的模板首跑」出现。
- **Q3 模板输入 schema**：固定 manifest 字段够不够撑起自动表单？要不要允许模板声明自定义 input schema（Windmill 式）？
- **Q4 RegisterWorkflow 语义**：同名不同 hash 时新建版本还是报错？保存为模板需要明确的 upsert 规则。
- **Q5 跨 workspace 枚举**：DB 按 workspace 隔离，面板如何发现「哪些 workspace 有工作流」——遍历已知 workspace 列表逐个查？
- **Q6 删除级联**：软删 workflow 时 versions/tasks 是否保留（建议保留，仅隐藏）。
- **Q7 user_id 字段**：workflows.user_id 当前闲置，单租户下是否继续忽略。
- **Q8 轨迹提取输入**：喂给模型的工具调用历史范围多大——整个会话还是仅本轮？事件中 tool_stdout 可能很大，是否只取 tool 名称/入参摘要/结果摘要，避免超上下文。
- **Q9 提取后的工具可用性**（已定）：保存前 dry-run compile 预校验工具存在性 + 运行期 fail-fast 兜底（`tool not found: X`，run 启动即失败，错误精确到工具名）。工具在提取会话可用、但运行 workspace 不可用（MCP 未连接/权限收紧）的场景，编译器直接拒绝保存并列出缺失工具。
- **Q10 headless ExecutionContext**（评审 B7）：无回合的 ToolRegistry/NestedExecutor/身份（workflowID 哈希输入）如何合成？automation Perm.Connectors/Skills 如何映射为工具资格集？
- **Q11 workspace-scoped 观测面 key**（评审 B2）：headless run 的快照/详情以 task_id 为 key 是否够？是否要 workflow run id 维度？
- **Q12 run 取消**（评审遗漏 1）：引擎是否补 Cancel API？面板 v1 是否只读不做取消？
- **Q13 触发鉴权**（评审遗漏 2）：直接触发端点的权限模型与防滥用（节流/配额）。
- **Q14 版本钉扎**（评审遗漏 3）：触发/automation 绑定模板的版本策略——最新版自动 vs 钉扎某版本。
- **Q15 overlap 判定与一致性**（评审 B5）：运行中状态存任务表哪个字段、由谁清理、多 daemon 是否一致；Buffer 队列存哪、上限多少。
- **Q16 tool_sequence 模板语法**（评审 B1，已定）：表达式基于引擎既有 expr-lang 语义——只允许引用 Nodes 内的节点名（否则 expr.Compile 报错）、可选字段一律 `?? default`（缺字段求值 nil 不报错）、InputMapping 与 Output.Extras 同一套校验、dry-run compile 统一拦截。**节点工具资格 = headless 注册表**（见 §6）：排除对话交互/工作流元管理/子代理/自动化管理四类，无额外 allowlist。
