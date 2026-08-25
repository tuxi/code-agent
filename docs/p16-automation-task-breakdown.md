# 自动化（Automation）需求确认表 + 子任务需求规格

关联文档：`docs/p15-scheduled-tasks-prd.md`（需求定义 PRD）
本文定位：把 PRD 的**需求确认点**完整提炼，据此拆分 T1-T5 并给出每个子任务的
**需求规格**、**验收标准**、**依赖关系**，作为实现前的对齐基线。

---

## 第一部分：需求定义确认表

从 PRD 提炼出的**本次实现前必须确认的需求点**。每一条是"以什么为准"，标注【已定】或【待确认】。
待确认项需拍板后才能进入 plan mode。

| ID | 需求确认点 | 结论 / 选项 | 状态 |
|---|---|---|---|
| R1 | 功能定名 | **Automation（自动化）**，不用 scheduled | 【已定】 |
| R2 | 库表命名 | `automations` / `automation_runs` / `automation_runtime_state` | 【已定】 |
| R3 | 引擎包 | `internal/automation` | 【已定】 |
| R4 | 工具包 | `internal/tools/automation`，工具名 `automation` + `get_current_time` | 【已定】 |
| R5 | 客户端端点 | `/v1/automations` 系列 | 【已定】 |
| R6 | 架构定性 | 自动化 = skill(说明) + 原生工具(执行) + 服务端引擎(触发) + 客户端控制面(展示) 四层；**不是纯 skill** | 【已定】 |
| R7 | 调度器位置 | 端侧必须放 `codeagentd` 进程内（无云兜底） | 【已定】 |
| R8 | 调度表达 | schedule_type(once/recurring) + rrule + scheduled_at 三字段分离 | 【已定】 |
| R9 | 触发时间物化 | next_run_at / last_run_at 用 INTEGER epoch 毫秒，ticker 整数比较 | 【已定】 |
| R10 | 热状态分离 | running/last_error/running_turn_id 独立成 `automation_runtime_state`(1:1) | 【已定】 |
| R11 | run=会话 | `automation_runs` 以 thread_id 为主键，read_at 当未读 | 【已定】* |
| R11a | **chat 模式 runs 归属** | chat 模式多次触发回同一会话，thread_id 会主键冲突。**待定**：A. 每次触发独立 run 记录，run 用独立 id，会话 id 另存；B. chat 模式不写 runs，只靠 runtime_state + 会话承载历史 | **待确认** |
| R12 | 工具形态 | A. 单工具多模式 `automation`(对齐 workbuddy automation_update)；B. 多工具拆分 | 待确认（建议 A） |
| R13 | 引擎落点 | 独立 `internal/automation` 包 | 待确认（建议独立包） |
| R14 | 「创建→填充输入框」UX | 属 chater/AgentKit 端，本期不实现，只约契约 | 待确认（建议本期不做） |
| R15 | daemon 重启补触发 | 跳过并标记 skipped（更安全） | 待确认（建议跳过） |
| R16 | 调度精度 | 固定 30s ticker | 待确认（建议固定 ticker） |
| R17 | 触发投递方式 | 复用 `conversation.TurnExecutor` 投 headless turn，不重复造并发控制 | 【已定】 |
| R18 | standalone 每 run 新建会话 | 复用 `repo.Create(workspacePath)` 新建会话；依赖 T1 的 store 也能新建 | 【已定】 |
| R19 | 权限模型 | 无人值守任务收窄权限；涉资金/下单"生成提案+等确认"，不默认执行 | 【已定】 |
| R20 | 时区处理 | 存 timezone，next_run_at 按创建时区物化 | 【已定】 |
| R21 | 多租户/外部推送字段 | 去掉 owner_* / push_to_wechat / push_to_wecom_bot | 【已定】 |
| R22 | 执行上下文注入 | cwds/model_id/skills/connectors → workspace/model/skill/MCP | 【已定】 |
| R23 | 控制面展示字段 | name/schedule 描述/status/next_run_at/last_status/run_count/prompt 摘要/未读数 | 【已定】 |

> **R11a 是唯一可能改变表结构的需求点**，直接影响 T1 的 `automation_runs` 主键设计，必须在 T1 开工前定。

---

## 第二部分：T1-T5 依赖关系总览

依赖方向：`A Depends On` = A 的实现需要 X（X 必须先就绪）；`B Blocked By` = B 被 Y 阻塞。

```
T1 数据模型/持久化  ──(Depends On)──> 无（最底层，最早开工）
      ▲
      │ T2 需要表 + store 接口
T2 调度引擎  ──(Depends On)──> T1； (Blocked By) 无
      ▲
      │ T3 需要 store 接口（用工具调用 T1 的 CRUD）
T3 管理工具  ──(Depends On)──> T1（store 接口）；(Blocked By) T2 不阻塞 T3
      ▲
      │ T4 需要 store 接口 = T1
T4 客户端端点  ──(Depends On)──> T1；(Blocked By) T2 不阻塞 T4
      ▲
      │ T5 需要工具真实存在（automation + get_current_time）
T5 skill 引导  ──(Depends On)──> T3（工具）；(Blocked By) T1 间接
```

### 关键依赖结论

- **T1 是唯一无上游依赖的根**，其余全部直接或间接依赖它。
- **T2、T3、T4 依赖 T1，但彼此互不阻塞**——可并行（T3 依赖 T1 的 store 接口，T4 依赖 T1，二者不依赖对方）。
- **T5 依赖 T3**（skill 描述的是真实工具），是最后一个，逻辑上最后做。

---

## 第三部分：各子任务需求规格与验收标准

### T1 — 自动化数据模型与持久化层

**负责**：
- 定义 `automations` / `automation_runs` / `automation_runtime_state` 三张表（PRD §7 schema）。
- 在存储层暴露一个 **AutomationStore 接口**（Create/Get/List/Update/Delete/ListRuns/RecordRun/UpdateRuntimeState/NextDueAt）。
- 提供 rrule → next_run_at 的解析、once 的 scheduled_at 校验、timezone 物化逻辑。
- 去掉 owner_*/push_* 字段；字段与 PRD §7 一致。

**不负责**：定时循环（T2）、模型工具（T3）、HTTP 端点（T4）、skill 内容（T5）。

**依赖**：None（根任务）。

**验收标准**：
1. 迁移后三张表存在，schema 与 PRD §7 一致（字段名、类型、索引）。
2. `AutomationStore.Create` 能插入且计算并返回首个 next_run_at（按 timezone 物化）。
3. `AutomationStore.Update` 未填字段保持不变（workbuddy 语义）。
4. 软删除：`Delete` 置 deleted_at/status，`List` 默认排除已删除。
5. rrule 解析：`FREQ=DAILY;BYHOUR=9` → 下一 9:00；`FREQ=MINUTELY;INTERVAL=30` → 下 30 分钟；`scheduled_at` once 精确到点。
6. `NextDueAt(now)` 返回 status=ACTIVE 且 deleted_at IS NULL 且 next_run_at<=now 的任务（供 T2 用）。
7. 单元测试覆盖上述解析/物化/软删除；`go test ./internal/automation/...` 通过。

---

### T2 — 自动化调度引擎（automation loop）

**负责**：
- `internal/automation` 起常驻循环：`time.Ticker`（30s），每 tick 调 `NextDueAt(now)`。
- 对每条到期任务：解析出其应投递的会话（standalone 新建 / chat 用 session_id），
  通过 `conversation.TurnExecutor` 投递一个 headless turn（消息 = prompt）。
- 投递后写 runtime_state（running/running_turn_id/last_run_at/last_error）、
  更新 automations.last_run_at 与下一次 next_run_at、写一条 automation_runs。
- 失败不阻塞循环：投递失败记 failed + last_error，继续下一个 tick。
- daemon 启动时 reconcile：对漏触发任务按 R15 标记 skipped。

**不负责**：表结构（T1）、工具（T3）、端点（T4）、skill（T5）。它消费 T1 的接口，不造表。

**依赖**：T1（AutomationStore 接口 + 表）。

**验收标准**：
1. 调度循环在 codeagentd 启动后运行，30s tick 检查到期任务。
2. 一次到期触发：投递一个 turn，写 runtime_state + runs 记录，更新 next_run_at。
3. standalone：每次触发**新建**一个会话（repo.Create(workspacePath)），run 为新会话。
4. chat：每次触发**回到** session_id 指定的会话（按 R11a 决定 runs 记录方式）。
5. 投递失败（会话不存在）→ 记录 failed + last_error，循环继续，不 panic。
6. 重启后对漏触发任务标记 skipped（不盲目补跑）。
7. 单元/集成测试：注入 fake store，验证"构造 time→触发→投递→写状态"闭环；`go test ./internal/automation/...` 通过。

---

### T3 — 自动化管理工具（automation + get_current_time）

**负责**：
- `internal/tools/automation` 实现 `tools.Tool` 接口的 `automation` 工具（mode=list/view/create/update/delete）。
- 实现 `get_current_time` 工具（返回 {now, timezone, utc_offset}）。
- 经 `runtime.BuildBaseRegistry` 注册进**基础 registry**（cfg.Agent.ToolAllowed 门控），故每个会话可用。
- `Execute` 通过 `tools.ExecutionContext` 上新增的 `AutomationStore` 接口访问 T1 存储。
- 参数校验：mode 必填；create 需 name/prompt/schedule_type/timezone；update 未填不变；once 需 scheduled_at，recurring 需 rrule。

**不负责**：表结构（T1）、循环（T2）、端点（T4）、skill 文案（T5）。

**依赖**：T1（AutomationStore 接口 + 表）。不依赖 T2（工具只做 CRUD，不触发循环）。

**验收标准**：
1. `automation` 工具注册进基础 registry，list/view/create/update/delete 五种 mode 可用。
2. create 返回 id + next_run_at；update 未填字段保持不变；delete 软删除。
3. once 与 recurring 校验：once 需 scheduled_at；recurring 需 rrule，否则报错。
4. `get_current_time` 返回正确 now/timezone/utc_offset。
5. 工具在无 AutomationStore 时优雅报错（不 panic）。
6. 单元测试覆盖五种 mode + 参数校验 + 时区工具；`go test ./internal/tools/automation/...` 通过。

---

### T4 — 客户端控制面端点（automations 路由）

**负责**：
- 在 `internal/server` 内注册 `/v1/automations` 系列路由（与 `/v1/conversations` 平行）。
- GET 列表 / POST 创建 / GET 详情 / PATCH 更新 / DELETE 软删 / GET runs。
- 列表返回 PRD §5.3 展示字段（name/schedule 描述/status/next_run_at/last_status/run_count/prompt 摘要/未读数）。
- 响应写 JSON（复用 writeJSON），错误码语义化。

**不负责**：表结构（T1）、循环（T2）、工具（T3）、skill（T5）。它消费 T1 的 store。

**依赖**：T1（AutomationStore 接口 + 表）。不依赖 T2（端点只读/写配置，不触发调度）。

**验收标准**：
1. 六个端点全部注册且方法正确（GET/POST/PATCH/DELETE）。
2. 列表返回摘要字段，含未读数（来自 runs.read_at）。
3. POST 创建、PATCH 更新（未填不变）、DELETE 软删生效。
4. GET 详情含最近 run；GET runs 返回历史。
5. 未授权/非法 id 返回正确状态码。
6. 集成测试：httptest 打各端点，验证 CRUD 闭环；`go test ./internal/server/...`（相关用例）通过。

---

### T5 — 自动化技能引导 skill（automation SKILL.md）

**负责**：
- 新建 `skills/automation/SKILL.md`（YAML frontmatter + 正文）。
- 覆盖 PRD §5.4 七点：触发词、先 get_current_time 认时区、自然语言→rrule/scheduled_at 映射、
  standalone vs chat、once vs recurring、cwds/skills/connectors、安全边界（涉资金生成提案不执行）。
- 复用现有 `internal/skills` 机制（PromptIndex 生成 L1 索引、load_skill 加载 body），不改机制。

**不负责**：表结构（T1）、循环（T2）、工具实现（T3）、端点（T4）。

**依赖**：T3（skill 描述的是真实存在的工具，必须在工具落地后写才不空转）。

**验收标准**：
1. `skills/automation/SKILL.md` 存在，frontmatter 含 name/description，能被 `skills.Load` 解析。
2. `load_skill automation` 能返回正文，且出现在 PromptIndex 索引。
3. 文案准确指向真实的 `automation` + `get_current_time` 工具（不虚构签名）。
4. 含时区、standalone/chat、once/recurring、安全边界的完整说明。
5. 单元测试：skill 成功加载、frontmatter 解析通过；`go test ./internal/skills/...` 通过。

---

## 第四部分：建设顺序建议（依赖驱动）

1. **先 T1**（无上游，唯一根）——定表结构 + AutomationStore 接口，并**先解决 R11a**。
2. **并行 T2 / T3 / T4**（都依赖 T1，彼此不阻塞）：
   - T2 引擎需要 T1；T3 工具需要 T1 的 store 接口；T4 端点需要 T1。
   - 工具 T3 与端点 T4 可同时做；它们不依赖 T2 的运行。但**要实现"自动触发"的体验闭环，需要 T2**。
3. **最后 T5**（依赖 T3 的工具真实存在）。

> 若追求"最小可用闭环"：先 T1 → T3（能创建）→ T4（能查看）→ T2（能自动触发）→ T5（引导）。
> 若纯并行最大化：T1 先动，T2/T3/T4 在 T1 的 AutomationStore 接口冻结后并行开工，T5 最后。

---

## 第五部分：进入 plan mode 前必须拍板的事项

- **R11a（cat 模式 runs 归属）**：唯一影响表结构的需求点，必须在 T1 定表前确认。
- **R12（工具形态）**：单工具多模式（建议）还是多工具拆分，决定 T3 的 schema 组织。
- **R13 引擎落点 / R14 UX / R15 补触发 / R16 精度**：建议值分别为独立包 / 本期不做 / 跳过 / 固定 ticker。

以上四点（R11a、R12、R13-R16）确认后，即可进入 plan mode 产出实现方案。
