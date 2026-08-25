# 自动化（Automation）功能 需求定义 (PRD)

状态：需求定义（非实现方案）
适用仓库：code-agent（端侧 Agent Runtime）
目标读者：Runtime 开发 / AgentKit SDK / chater 宿主 / agent-gateway

---

## 1. 背景与核心矛盾

**目标一句话**：让用户能用一句自然语言创建"自动化"（Automation），模型通过原生工具把它持久化到本地，
daemon 按调度规则在后台触发，把每次运行结果经事件流送回对应会话，并能让客户端
通过一个**控制面面板**查看全部任务、创建任务。

**命名决策**：本功能定名 **Automation（自动化）**，不用 scheduled。
`scheduled` 读起来是"排期机制"，`automation` 才是产品意图——一条按规则自动跑的活。
因此库表用 `automations`，引擎包用 `internal/automation`，工具包用 `internal/tools/automation`，
端点是 `/v1/automations`。

**为什么不能照搬 Codex**：Codex 的定时任务本体跑在 OpenAI 云后端，端侧只保证"需要本地文件时开机+开 App"。
code-agent 是**端侧** agent：没有云服务器兜底，唯一能"在你不操作时还活着"的进程是本地常驻的
`codeagentd`。因此调度器必须放进 `codeagentd` 进程内。

**现有 gap**：code-agent 目前只有"准入并发调度"（`conversation.TurnScheduler`：同一会话同时一个 turn、
进程级并发上限、排队），它跟"到点了该跑什么"无关。`internal/jobs` 只是后台跑一条 build/test 命令。
**没有任何时间/周期触发的组件**，也没有自动化的持久化表、模型工具、或客户端端点。本次全部是新增面。

---

## 2. 关键概念（术语）

| 术语 | 定义 |
|---|---|
| **Automation** | 一条持久化的自动化定义：做什么(prompt) + 何时跑(schedule) + 跑在哪(cwds/model/mode) |
| **Run** | 一次实际触发。每次 run 是一次独立 agent turn；standalone 模式下一次 run 等于一个新会话 |
| **Standalone（独立任务）** | 每次触发都从保存的 prompt 起一个新会话/新上下文，结果作为独立 run 呈现（对齐 Codex "standalone scheduled task"） |
| **Chat（回话任务）** | 每次触发都回到同一会话，带上既有上下文继续（对齐 Codex "scheduled task inside a chat"） |
| **schedule_type** | `once`（一次性）或 `recurring`（周期性），二者用不同字段表达 |
| **rrule** | recurring 的 RFC 5545 规则，如 `RRULE:FREQ=DAILY;BYHOUR=20;BYMINUTE=0` |
| **scheduled_at** | once 的一次性时间点（ISO / epoch），非一次性为空 |
| **next_run_at / last_run_at** | 物化的 epoch 毫秒，ticker 直接与 now 比较，不实时解析 RRULE |
| **Native Tool** | 模型可直接调用的结构化能力（如 `automation`）。确定性，不依赖模型读 skill |
| **Skill** | 教模型"何时该用工具、自然语言如何映射成工具参数"的说明书。不执行操作 |

**本需求最重要的判定**：自动化**不是一个 skill**，而是"**skill（说明书）+ 原生工具（执行能力）
+ 服务端引擎（真正触发）+ 客户端控制面（展示）**"四层。skill 永远不能代替调度器——模型必须被
唤醒才可能有"到点"概念，模型本身不携带时钟。真正"到点触发"的是 daemon 内的调度循环。

---

## 3. 需求范围

### 3.1 范围内 (In Scope)

1. **服务端调度引擎**：常驻调度循环，周期性检查到期任务并投递 turn（`internal/automation`）。
2. **模型原生工具**：在任何会话里都能做自动化的增删改查（`automation`）+ 读取当前时间（`get_current_time`）。
3. **客户端控制面面板**：查看全部任务状态、创建任务、暂停/启用、查看详情与历史 run 的 REST 端点。
4. **Skill 引导**：一份 `automation` skill，教模型怎么把自然语言翻译成工具参数。
5. **持久化**：自动化定义 + 每次运行历史 + 运行时热状态，存本地 SQLite。
6. **权限模型**：无人值守任务的沙箱/审批收窄策略。

### 3.2 范围外 (Out of Scope)

- ❌ 云端调度 / 关机能触发。端侧做不到，也不做。
- ❌ **默认真实交易下单**（见 §6 安全模型）。端侧只做"监控+预警+生成可执行指令"，不下单。
- ❌ 跨设备同步（不做云同步）。
- ❌ 复杂的图形化排班编辑器（本期只做 rrule/scheduled_at + 自然语言）。
- ❌ 「创建→填充输入框」的客户端交互属 chater/AgentKit 端，本期只定义契约，不实现 UI。
- ❌ 外部推送渠道（workbuddy 的 push_to_wechat / push_to_wecom_bot）——code-agent 无对应连接器，
  作为未来扩展点记录，不实现。

---

## 4. 典型场景（用户故事）

**场景 A —— 每日科技股行情（核心）**
> "每天下午 4 点帮我整理科技行业股票今日行情，含涨跌幅、成交量、重点关注个股；简要分析涨跌原因；列 1-2 个值得关注的事件。"

模型调用 `automation`(mode=create)，schedule_type=recurring，rrule=`FREQ=DAILY;BYHOUR=16;BYMINUTE=0`，
timezone=创建时区，prompt=上文，mode_exec=standalone。到点触发 turn，用 web_search/web_fetch 拉数据，
结果作为 run 显示在控制面。

**场景 B —— BTC 价格监控 / 预警（非下单）**
> "每 30 分钟看一次 BTC 价格，跌破 60000 就提醒我。"

模型创建 recurring 任务（rrule=`FREQ=MINUTELY;INTERVAL=30`）。到点触发 turn 拉当前价，只有满足条件
才"提醒"，否则标记本次 run"无需报告"。**绝不自动下单。**

**场景 C —— 回话式跟进**
> "帮我盯着这次部署，每 5 分钟检查一次，有任何失败就回到这个对话告诉我。"

mode_exec=chat，每次都回到同一会话带上下文继续，适合长时轮询。

**场景 D —— 一次性任务**
> "明天下午 3 点提醒我开会"

schedule_type=once，scheduled_at=具体 ISO 8601 时间点。

---

## 5. 四大模块需求定义

### 5.1 服务端调度引擎（新增 `internal/automation` 包）

**定位**：`conversation.TurnScheduler`（准入并发控制）、`internal/jobs`（后台命令）、
`internal/automation`（时间/周期触发）是**三个正交组件**，互不替代。

**职责**：
- 持有 `time.Ticker`（粒度建议 30s，可配置），每个 tick 查一次到期任务。
- 查询：`SELECT * FROM automations WHERE deleted_at IS NULL AND status='ACTIVE' AND next_run_at <= now_millis`。
- 对每条到期任务：复用 `conversation.TurnExecutor` 投递一个 headless turn（消息 = prompt）。
- 投递后写 `automation_runtime_state`（running / running_conversation_id / last_run_at / last_error），
  更新 `automations.last_run_at` 与下一次 `next_run_at`，写一条 `automation_runs`。
- **不重复造并发控制**：投递走 executor 主路径，触发过多时空然由现有 `TurnScheduler` 排队。

**时区处理（关键，豆包专门强调）**：
- 存储 timezone（创建时区）。`next_run_at` 按该时区计算并物化为 epoch 毫秒。
- ticker 只做 `next_run_at <= now_millis` 比较（本地墙钟），不实时解析 RRULE，抗时钟回拨、开销低。
- 首次触发 = 依据创建时区计算的下一匹配点。

**失败与恢复**：
- 投递失败（会话不存在等）→ 写 `automation_runs.status=failed`，`runtime_state.last_error` 更新，不阻塞循环。
- daemon 重启 → 启动时扫 `status='ACTIVE' AND deleted_at IS NULL AND next_run_at <= now` 的任务，
  按 Q4 决策跳过并标记 skipped 或补触发（建议跳过，见 §9 Q4）。

### 5.2 模型原生工具（新增 `internal/tools/automation`）

**工具 1：`automation`** —— 自动化 CRUD 主工具（等价 workbuddy 的 `automation_update`）。
单工具多模式（mode 分派），注册进**基础 registry**，故每个会话都可见、可调用：

```
mode: list | view | create | update | delete
```
- `list`：返回摘要（id/name/status/next_run_at/last_status/run_count）——喂控制面列表。
- `view`：返回完整配置。
- `create`：落库，返回 id + 计算好的 next_run_at。
- `update`：按 id 更新，**未填字段保持不变**（workbuddy 明确语义）。
- `delete`：软删除（deleted_at / status），符合安全模型。

create/update 输入字段：
```json
{
  "mode": "create",
  "name": "每日科技股行情",
  "prompt": "每天下午4点...",
  "schedule_type": "once|recurring",
  "rrule": "FREQ=DAILY;BYHOUR=16;BYMINUTE=0",
  "scheduled_at": "2026-08-26T15:00:00-07:00",
  "timezone": "America/Los_Angeles",
  "mode_exec": "standalone|chat",
  "session_id": "",              // chat 模式必填回哪个会话；standalone 可空
  "cwds": [],                    // 可选，工作目录数组
  "model_id": "",                // 可选，用哪个模型跑
  "skills": [],                  // 可选，运行时要自动加载的 skill 名
  "connectors": [],              // 可选，运行时要启用的 MCP server 名
  "permission_mode": "",         // 可选，run 的沙箱/审批模式
  "valid_from": "", "valid_until": "",
  "enabled": true
}
```

**工具 2：`get_current_time`** —— 时间原语（豆包专门 `tool_search` 找它）。
返回 `{now, timezone, utc_offset}`。模型建任务前先确认当前时区，再据此填 `timezone`，
从源头解决"rrule 在服务端 UTC 与用户 PDT 差 7 小时"的问题。

**注册方式**：实现 `tools.Tool` 接口（Name/Description/InputSchema/Execute），经
`Registry.Register` 注册进基础 registry（`runtime.BuildBaseRegistry`）。`Execute` 通过
`tools.ExecutionContext` 访问自动化存储（新增 `AutomationStore` 接口字段）。

### 5.3 客户端控制面（`internal/server` 内，与 `/v1/conversations` 平行）

复用 `mux.HandleFunc("METHOD /path", ...)` 注册：

| 端点 | 方法 | 作用 |
|---|---|---|
| `/v1/automations` | GET | 任务列表（摘要），客户端渲染「进行中/已暂停」 |
| `/v1/automations` | POST | 创建任务 |
| `/v1/automations/{id}` | GET | 查看单个任务详情 + 最近 run |
| `/v1/automations/{id}` | PATCH | 更新（暂停/启用/改 schedule） |
| `/v1/automations/{id}` | DELETE | 删除（软删除） |
| `/v1/automations/{id}/runs` | GET | 该任务历史 run |

**列表展示字段**（对齐豆包「进行中」卡片 + Codex `Scheduled` 收件箱）：
`name`, schedule 人类可读描述（"每天 16:00"）, `status`, `next_run_at`, `last_status`,
`run_count`, `prompt` 摘要, 未读数（来自 `automation_runs.read_at`）。

**实时反馈**：每次到点触发的 turn 已走 executor 主路径，其事件流（thinking/tool_stream/结果）
自然进入会话的 event store，客户端可在既有 WS stream 上看到。任务列表刷新用
"客户端动作后 GET" 或新增一个轻量 WS 事件（本期按 GET 刷新即可）。

**「创建→填充输入框」引导 UX**：纯客户端（chater/AgentKit）行为，对应 Codex 截图——
点「创建」时客户端把一段引导 prompt 预填进输入框（"我来帮你建自动化，请告诉我：要做什么？
什么时候运行？每次回到本对话还是新开对话？"），走对话式确认后才 POST `/v1/automations`。
**不涉及服务端**，本期只约定非阻塞前提。

### 5.4 Skill 引导（新增 `skills/automation/SKILL.md`）

**作用**：教模型"何时该用工具、自然语言如何映射成工具参数"。不执行任何操作。

**SKILL.md 必须覆盖**：
1. 触发词：用户提到"每…/每天…/每周…/定时/提醒/监控/自动"等。
2. **先调用 `get_current_time`** 确认时区（豆包核心流程），再据此填 `timezone`。
3. 自然语言 → rrule/scheduled_at 映射：每天9点→`FREQ=DAILY;BYHOUR=9`；每周一→`FREQ=WEEKLY;BYDAY=MO`；
   每30分钟→`FREQ=MINUTELY;INTERVAL=30`；明天下午3点→scheduled_at 具体时间。
4. 区分 `standalone` 与 `chat`：独立事件用 standalone；持续跟进/回话用 chat。
5. 一次性 vs 周期性（schedule_type）。
6. 涉及项目文件时填 `cwds`；涉及 skill/MCP 时填 `skills`/`connectors`。
7. **安全边界**：无人值守任务收窄权限；涉及资金/下单只能"生成提案+等确认"，不得默认执行。

**注册机制**：复用现有 `internal/skills`（`skills/` 目录 + YAML frontmatter，
`PromptIndex` 生成 L1 索引、`load_skill` 加载 L2 body）。无需改机制，新增文件即生效。

---

## 6. 安全与审批模型

- 无人值守任务在无人类在场时运行，**必须收窄权限**（对齐 Codex 官方安全模型：后台任务
  默认 `approval_policy="never"` + 最小沙箱；full access 携带明显风险被排除）。
- 沙箱模式建议：read-only / workspace-write 起步，按需 allowlist 命令（现有 `Rules`）。
  每次 run 的 `permission_mode` 记录在 automations 行，供 run 时采用。
- **授权边界（明确）**：
  - 允许：读行情、跑 web_search/web_fetch、生成分析/报告/预警、生成"可一键执行的指令"。
  - 不允许：未经用户确认的真实下单/资金操作。此类操作要么配置为"生成提案+等确认"，
    要么直接 out of scope 留给未来的 connector 授权。
- 每条 run 的审计：`automation_runs` 记录 status/result_success/result_summary，客户端可回看。

---

## 7. 数据存储与持久化

复用现有 SQLite（加表，不新建库）。**schema 借鉴 workbuddy 的三表设计**（`~/.workbuddy-ai/workbuddy.db`
的 `automations` / `automation_runtime_state` / `automation_runs`），去掉其多租户与外部推送字段，
改造成 code-agent 可用的形态。

```sql
-- 定义行：只放稳定配置
CREATE TABLE automations (
  id              TEXT PRIMARY KEY,            -- task_<nano>_<seq>
  name            TEXT NOT NULL,
  prompt          TEXT NOT NULL,
  status          TEXT NOT NULL,               -- ACTIVE | PAUSED
  schedule_type   TEXT NOT NULL DEFAULT 'recurring',  -- once | recurring
  rrule           TEXT NOT NULL DEFAULT '',    -- recurring 用 RFC5545
  scheduled_at    TEXT,                        -- once 用一次性时间点，非 once 为空
  timezone        TEXT NOT NULL,               -- 创建时区
  mode_exec       TEXT NOT NULL DEFAULT 'standalone', -- standalone | chat
  session_id      TEXT,                        -- chat 模式回哪个会话；standalone 可空
  cwds            TEXT NOT NULL DEFAULT '[]',  -- JSON 数组，工作目录
  model_id        TEXT,                        -- 可选，用哪个模型跑
  skills_json     TEXT NOT NULL DEFAULT '[]',  -- JSON 数组，运行时自动加载的 skill
  connector_ids_json TEXT NOT NULL DEFAULT '[]', -- JSON 数组，运行时要启用的 MCP
  permission_mode TEXT,                        -- 可选，run 的沙箱/审批模式
  valid_from      TEXT, valid_until TEXT,
  last_run_at     INTEGER, next_run_at INTEGER, -- epoch 毫秒，物化
  run_count       INTEGER NOT NULL DEFAULT 0,
  last_status     TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  deleted_at      INTEGER
);
CREATE INDEX idx_automations_next_run ON automations(status, deleted_at, next_run_at);

-- 热状态：1:1 与定义行，运行中/错误等热点可变字段不进定义行
CREATE TABLE automation_runtime_state (
  automation_id          TEXT PRIMARY KEY,
  last_run_at            INTEGER,
  last_error             TEXT,
  running                INTEGER NOT NULL DEFAULT 0,
  running_started_at     INTEGER,
  running_turn_id        TEXT
);

-- 运行历史：一次 run = 一个会话/线程，thread_id 即会话身份
CREATE TABLE automation_runs (
  thread_id       TEXT PRIMARY KEY,
  automation_id   TEXT NOT NULL,
  status          TEXT NOT NULL,               -- running|succeeded|failed|skipped
  read_at         INTEGER,                     -- 未读指示（收件箱）
  thread_title    TEXT,
  source_cwd      TEXT,
  result_success  INTEGER,
  result_summary  TEXT,
  created_at      INTEGER NOT NULL
);
```

**设计说明（借鉴自 workbuddy）**：
- `schedule_type` + `rrule` + `scheduled_at` 三字段分离：once 与 recurring 语义天然不同，拆开无歧义。
- `next_run_at`/`last_run_at` 用 INTEGER epoch 毫秒物化，ticker 只做整数比较。
- 热状态（running/last_error/running_turn_id）独立成 `automation_runtime_state`（1:1），
  避免每次 tick 写定义行、避免锁住定义行。
- `automation_runs` 以 `thread_id` 为主键——standalone run 每次新建会话，run 身份即会话 id，
  `read_at` 直接当未读标记（对接 Codex 收件箱 + 豆包进行中）。
- 去掉 workbuddy 的 `owner_user_id/owner_status/owner_source`（多租户，单机不需要）与
  `push_to_wechat/push_to_wecom_bot`（无对应连接器）。

---

## 8. 验收标准（粗粒度，供后续细化）

1. 模型能用一句自然语言创建 recurring/once 自动化，且正确解析时区。
2. 创建的 turn 在 `next_run_at` 到点被 daemon 自动触发，无需人工干预。
3. 触发后结果写回对应会话（standalone 新建 / chat 回原会话），事件流可看。
4. 客户端能 GET 任务列表、暂停/启用、查看详情与历史 run。
5. `get_current_time` 正确返回本地时区与 UTC 偏移。
6. 时区正确性：同一条 rrule 在 PDT（20:00）与 UTC（次日03:00）语义独立、互不串扰。
7. 无人值守任务遵循收窄的权限策略；涉及下单指令默认生成提案而非执行。

---

## 9. 待确认的开放问题（实现前需拍板）

| # | 问题 | 选项 | 建议 |
|---|---|---|---|
| Q1 | 模型工具形态 | A. 单工具多模式 `automation`（对齐 workbuddy `automation_update`）；B. 多工具拆分 `create_automation`/`list_automations` 等 | A：模型只认一个工具，参数靠 mode；与 workbuddy 一致 |
| Q2 | 引擎落点 | A. 独立 `internal/automation` 包；B. 塞进 `conversation` 内部 | A：横跨 持久化+工具+端点+引擎，独立包更干净 |
| Q3 | 「创建→填充输入框」引导 UX 是否进本期 | A. 进；B. 不进，只落服务端能力 | 先 B，服务端契约先行；引导 UX 属 chater 端 |
| Q4 | daemon 重启后补触发策略 | A. 重算并补触发错过的到期任务；B. 跳过并标记 skipped | B 更安全（避免突发的无人值守积压） |
| Q5 | 调度精度 | A. 固定 30s ticker；B. 按最近 next_run_at 设 sleep 精确到秒 | A 简单够用；B 更省唤醒但需处理时钟回拨 |
