---
name: automation
version: "1"
description: Create, list, view, update, and delete scheduled automations (定时任务/自动化) using the `automation` tool. Use when the user says "每…/每天…/每周…/定时/提醒/监控/自动/automation/定时任务/自动化", or asks to run something on a schedule, monitor something periodically, or get a recurring report. Covers natural-language → rrule mapping, timezone handling, standalone vs chat mode, and the safety boundary for unattended tasks.
---

# Automation（定时任务 / 自动化）

用户想"到点自动跑一件事"时，用 `automation` 工具把它持久化，daemon 会在后台按调度触发。
**你只负责把自然语言翻译成工具参数并调用工具**——真正"到点触发"的是 daemon 内的调度循环，
不是 skill，也不是模型本身（模型不携带时钟）。

## 触发词

用户提到以下任一意图，就应创建/管理自动化：

- 周期："每天/每周/每月/每30分钟/每天早上9点/每周一"
- 一次性："明天下午3点/今晚8点/下周一"
- 意图："提醒我/帮我盯着/监控/自动整理/定时推送/定期总结/到点叫我"

## 创建流程（必须按顺序）

1. **先调用 `get_current_time`** 确认当前时区（返回 `timezone` 字段）。
   时区决定调度语义：同一 rrule 在 UTC 与 PDT 差 7 小时。把返回的 `timezone` 原样填入
   `automation` 工具的 `timezone` 参数。
2. **确认关键信息（对齐 WorkBuddy 的对话式创建）**：如果用户没有一次性给全以下信息，
   **先调用 `ask_user` 逐项确认**，不要直接创建：
   - **任务内容**（prompt）：用户只说"11点开始"没说做什么时，必须问。给 2-4 个常见选项
     （如"总结今日工作 / 拉取项目状态 / 生成日报模板 / Other"）。
   - **工作目录**（cwds）：涉及项目文件时问"在哪个工作目录运行？"（选项：当前工作区 / 无特定目录 / Other）。
   - **权限**：任务要操作敏感内容（资金、外部平台、写文件）时，问权限级别
     （选项：默认权限 / Full access / 仅指定连接器免确认）。
   用户回答后，把答案填入对应字段再创建。
3. 调用 `automation` 工具，`mode=create`，填好 name / prompt / schedule_type / rrule 或
   scheduled_at / timezone / mode_exec /（可选）cwds / permission_mode / connectors。
4. 创建成功后向用户确认：任务名、执行时间、首次触发时间（工具返回的 `next_run_at`）、
   工作目录、权限级别。一次性任务要说明"执行一次后自动结束"。

## 自然语言 → 调度规则映射

| 用户说 | schedule_type | rrule / scheduled_at |
|---|---|---|
| 每天下午4点 | recurring | `FREQ=DAILY;BYHOUR=16;BYMINUTE=0` |
| 每天早上9点 | recurring | `FREQ=DAILY;BYHOUR=9;BYMINUTE=0` |
| 每周一早上9点 | recurring | `FREQ=WEEKLY;BYDAY=MO;BYHOUR=9;BYMINUTE=0` |
| 每30分钟 | recurring | `FREQ=MINUTELY;INTERVAL=30` |
| 每小时 | recurring | `FREQ=HOURLY;INTERVAL=1` |
| 每月1号9点 | recurring | `FREQ=MONTHLY;BYHOUR=9;BYMINUTE=0`（按创建日对齐） |
| 明天下午3点 | once | scheduled_at = 具体 RFC3339 时间（含时区偏移） |

- 未指定小时/分钟时默认 00:00。
- 复杂规则（BYMONTH/BYSETPOS/COUNT/UNTIL）当前不支持，遇到就明确告知用户，不要硬造。

## standalone vs chat（mode_exec）

- **standalone（默认）**：每次触发新建一个会话，从 prompt 独立执行。适合"每日行情报告"这类
  每次独立产出的任务。
- **chat**：每次触发回到同一会话（需填 `session_id`），带上既有上下文继续。适合"盯着这次部署，
  有失败就回到这个对话告诉我"这类持续跟进。
- 用户没明说时，默认 standalone。

## 涉及项目文件时

- 任务要操作某个项目 → 填 `cwds`（工作目录数组）。
- 任务要用某个 skill / MCP 连接器 → 填 `skills` / `connectors`。
- 不填时，standalone 任务会落在创建者会话所在的 workspace。

## 管理操作

- 查看全部：`automation` mode=list（摘要）。
- 查看详情：mode=view + id。
- 暂停/启用：mode=update + id + `enabled:false/true`。
- 改时间/内容：mode=update + id + 只传要改的字段（未传字段保持不变）。
- 删除：mode=delete + id（软删除）。

## 安全边界（必须遵守）

无人值守任务在无人类在场时运行，**必须收窄权限**：

- 允许：读行情、web_search/web_fetch、生成分析/报告/预警、生成"可一键执行的指令"。
- **禁止**：未经用户确认的真实下单/资金操作。涉及资金/交易的任务，只能"生成提案 + 等用户确认"，
  绝不默认执行。
- 创建涉及敏感操作的任务前，提醒用户该任务会无人值守运行，并建议收窄权限。
- **per-task 权限**：任务创建时通过 `permission_mode` / `connectors` 显式声明权限级别——
  - `permission_mode=full_access`：该任务触发时所有工具免确认（仅当用户明确要求"全权处理"时用）。
  - `connectors=[...]`：仅列出的 MCP 连接器在该任务触发时免确认，其余工具仍走默认审批。
  - 不填：该任务触发时走会话默认审批（最安全）。
  - 创建时用 `ask_user` 让用户明确选择权限级别，不要替用户默认 full_access。

## 示例

用户："每天下午4点帮我整理科技行业股票今日行情"

1. `get_current_time` → timezone=America/Los_Angeles
2. `automation` mode=create:
   - name: 每日科技股行情
   - prompt: 整理科技行业股票今日行情，含涨跌幅、成交量、重点关注个股；简要分析涨跌原因；列 1-2 个值得关注的事件
   - schedule_type: recurring
   - rrule: `FREQ=DAILY;BYHOUR=16;BYMINUTE=0`
   - timezone: America/Los_Angeles
   - mode_exec: standalone
3. 向用户确认创建成功与首次触发时间。