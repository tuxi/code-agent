# code-agent `/goal` 功能需求文档

> 版本：v0.1（待评估）
> 目标读者：负责实现的开发
> 目的：明确 `/goal` 要解决的问题、范围边界、接口与验收标准，供评估可行性与工作量。

---

## 1. 背景与要解决的问题

code-agent 当前是一问一答（turn-by-turn）模式：用户给一个较大的任务，agent 做一步就把控制权交还给用户，用户需要反复输入「继续」直到任务完成。对「比一个 prompt 大、但有明确终点」的任务，这种交互成本很高。

`/goal` 要解决的**不是**能力问题，而是**续航问题**：让 agent 围绕一个**可验证的完成条件**自主循环（plan → act → test → check），直到条件满足、被暂停、超预算或无可行路径为止，期间不需要用户逐轮催促。

一句话定义：`/goal` 是「目标比一个 prompt 大、有一条机器能判的终点线、且逼近终点的过程是 try-test-adjust」这一类任务的自主续航机制。

---

## 2. 范围

### 2.1 在范围内（适用目标的特征）

一个目标只有同时满足以下三点，才应被 `/goal` 接受：

1. 工作量大于单轮（否则普通 prompt 即可）。
2. 有客观、可机检的终点（测试通过 / build 退出码为 0 / 覆盖率达阈值 / 队列清空 / eval 分数达标等）。
3. 逼近终点的过程是机械迭代（改一版、验一版、再改）。

典型用例：模块迁移到新 API 直至全部调用点编译通过且测试绿；按设计文档实现到验收点全部成立；提升测试覆盖率到阈值；先写复现测试再循环修 bug 至其变绿；拆大文件至每个低于行数预算；清空打标签的 issue backlog；对照 eval 套件迭代 prompt 至分数达标。

### 2.2 明确不在范围内

- **无可验证终点的目标**：如「帮我赚钱」「把代码写优雅点」。判据：若连一条 verify 命令或一个可机检的判定都写不出来，则该目标不被接受（见 §4.7 准入判定）。
- **需要在真实世界执行高风险动作的目标**：动账户、收付款、发布对外内容、删除数据等。这些动作即便出现在目标描述里也不得自动执行。
- **跨 session 的全局记忆/调度**：`/goal` 只维护单一线程内的目标推进，不负责「明天 9 点跑」这类定时调度。

---

## 3. 核心概念与术语

| 术语 | 含义 |
|---|---|
| Goal | 持久化的线程状态（带 ID），而非一条 prompt。包含目标条件、状态、预算、已耗用量。 |
| Objective | 目标的完成条件文本，建议上限 4000 字符；更长的指令放文件里再引用。 |
| Worker | 执行一次朝目标推进的 turn 的组件（包装现有 agent 单轮循环）。 |
| Checker | 判定目标是否达成的组件。**与 Worker 分离，绝不让 Worker 自评。** |
| Transcript / Evidence | 会话视图；Evidence 仅返回 Worker 显式落下的可验证内容（测试输出、build 日志、git status 等）。 |
| Budget / Spend | 预算上限（turns / tokens / wall-clock）与已耗用量。 |

---

## 4. 功能需求

### 4.1 目标生命周期命令（REPL）

- `/goal <objective>`：设定并立即开始推进。设定前执行准入判定（§4.7）。
- `/goal`（无参）：查看当前目标、状态、已耗 turns / tokens / wall-clock。
- `/goal pause`：在**当前 turn 的边界**优雅暂停（不打断进行中的工具调用），落盘状态。
- `/goal resume`：从落盘状态恢复推进。
- `/goal clear`：清除当前目标。

### 4.2 推进主循环（Pursue Loop）

每一轮循环：

1. 检查是否被取消/暂停 → 若是，落盘为 `paused` 并返回。
2. 检查是否超预算 → 若是，落盘为 `budget_limited` 并返回。
3. 调用 Worker 执行一个 turn，累加 turns 与 tokens。
4. 调用 Checker 判定，记录判定理由（`LastNote`）。
5. 分支：`blocked` → 落盘返回；`met` → 状态置 `achieved` 自动清除并返回；否则落盘进度后进入下一轮。

### 4.3 Checker：确定性优先，LLM 兜底

- **默认使用确定性 checker**：给定一条 verify 命令（如 `go test ./...`），按退出码判定（0 = 达成）。确定、零模型成本、不会自我放水。
- **LLM checker 仅用于无法用命令判定的模糊条件**，且必须满足：
    - 使用**独立于 Worker 的便宜模型**（如 DeepSeek 小号模型），不得与 Worker 同模型。
    - 上下文只喂目标条件 + `Evidence()`，**不给整段对话、不让它跑命令**。
    - 要求严格 JSON 输出 `{"met":bool,"blocked":bool,"reason":string}`，证据不足时判未满足。
- Checker 设计为可插拔接口，便于后续扩展（如复合 checker：命令 + LLM 双重判定）。

### 4.4 预算与停机

- 支持 `MaxTurns` / `MaxTokens` / `MaxWall` 三类预算，任一触顶即进入 `budget_limited`。
- 预算是必选项，不允许无上限运行（成本与失控保护）。

### 4.5 持久化与恢复

- Goal 状态必须可存可读，使 server-side session 中断后可 `resume`。
- 复用现有 session history 的存储；Goal 与所属 session 同生命周期。

### 4.6 可观测性

- 推进过程中对外暴露实时进度：已耗 turns、tokens、wall-clock，以及最近一次判定理由。
- 进入 `blocked` / `budget_limited` 时，向用户清晰说明原因与下一步建议。

### 4.7 准入判定（设定目标时）

设定目标时进行准入判断：能否为该目标写出一条 verify 命令或一个可机检判定？

- 能 → 接受，挂确定性 checker。
- 不能但属于「可由证据判定的模糊条件」→ 接受，挂 LLM checker，并提示用户其判定不如命令可靠。
- 两者皆否（无任何可验证终点，或涉及高风险动作）→ **拒绝**，并向用户说明为何该目标不适合 `/goal`。

---

## 5. 状态机

```
                ┌─────────┐
   /goal <obj>  │ active  │
  ───────────▶  └────┬────┘
                     │
   ┌─────────────────┼──────────────────────────┐
   │                 │                           │
 met            not met / loop              pause / cancel
   │                 │                           │
   ▼                 ▼                           ▼
┌──────────┐   (回到 active 下一轮)         ┌────────┐
│ achieved │                               │ paused │──/goal resume──▶ active
│(自动清除)│                               └────────┘
└──────────┘
   超预算 ──▶ budget_limited
   无可行路径 ──▶ blocked        （二者均可由用户重新 resume 或 clear）
   /goal clear ──▶ cleared
```

合法状态：`active` / `achieved` / `paused` / `budget_limited` / `blocked` / `cleared`。

---

## 6. 接口设计（建议，供评估）

以下为建议的 Go 接口形态，开发可据实调整。关键约束是 Worker / Checker / Store 三者解耦。

- `Worker.RunTurn(ctx, goal, transcript) (TurnResult, error)`：包装现有 agent 单轮循环。
- `Checker.Check(ctx, goal, transcript) (CheckResult, error)`：返回 `{Met, Blocked, Reason}`。
    - 实现一 `DeterministicChecker{VerifyCmd []string}`。
    - 实现二 `LLMChecker{Model string, Client LLMClient}`。
- `Store.Save / Load`：持久化 Goal。
- `Transcript.Evidence() string`：仅返回可验证证据。

（已有一份可运行骨架 `goal.go` 可作为起点。）

---

## 7. 与现有系统的集成点

1. **Worker** 包装现有 REPL/agent 单轮循环，不重写。
2. **Store** 复用现有 session history 存储，Goal 与 session 同存。
3. **模型路由**：LLM checker 走现有 DeepSeek 路由，但选用独立小号模型。
4. **token 双线（修正）**：`Spend.Tokens` 是**累计消耗（counter）**，需在 agent loop 内对本 turn 每次 `resp.Usage`（prompt+completion）累加，经 `agent.TurnResult.TokensUsed` 暴露；compaction 触发看的是**瞬时上下文大小（gauge）**，继续用 `sess.PromptTokens` 不变。两者共享的只是 token 数字的来源（`resp.Usage`），**不是同一个累加器**。（旧版「共用同一套计量」措辞不准确，已废止。）
5. **compaction 与 Evidence 的咬合（重点，Phase 3）**：Evidence 契约固定为「最近的、白名单内的工具结果，每条尾部截断」。Phase 1 在 check 时直接读 `sess.Messages`（零新增写路径）；read-live 在 compaction 重写/丢弃旧 tool 消息后会缩水，Phase 3 通过「goal-aware compactor 保留证据窗口」或「emit 时快照进 goal 自有 buffer」解决。换捕获方式只改 compaction 下的保证，不改契约签名。
6. **auto mode 是 `/goal` 无人值守路径的上游前置**：审批策略是全局关注点，不放进 goal 包。`/goal` 内核可独立交付；但在 auto mode 落地前，`/goal` 跑起来仍会停在 `ConfirmApprover` 的 y/N，无法真正 hands-off。两者是依赖关系、不是同一个 PR。

---

## 8. 验收标准

1. 设定一个「测试通过」类目标后，agent 能在无人工催促下连续推进多轮，并在测试变绿时自动 `achieved` 并交还控制权。
2. 设定一个无法写出 verify 命令、且无可验证终点的目标（如「赚钱」），系统在准入阶段拒绝并说明原因。
3. `pause` 能在 turn 边界优雅停止、落盘；`resume` 能从落盘状态正确续跑，turns/tokens 计数连续不丢。
4. 任一预算触顶时进入 `budget_limited` 并停止，不超额烧 token。
5. LLM checker 使用的模型与 Worker 不同，且其上下文不包含整段对话、只含目标条件与 Evidence。
6. server-side 场景下 session 中断后，Goal 状态可被重新 `Load` 并 `resume`。

---

## 9. 风险与开放问题（请开发重点评估）

1. **暂停粒度**：「turn 边界优雅暂停」如何与进行中的工具调用/子进程协调？是否需要让 Worker 支持可取消的 ctx 并在工具调用之间检查取消？
2. **compaction 后的证据保留**：哪些内容算「可验证证据」、保留多久、如何与压缩策略协调（与 Phase 3 强相关）。
3. **目标投机满足（gaming）**：如何在条件层面约束「不准改测试文件 / 不准 hardcode 预期输出」？是否在准入时引导用户补充约束。
4. **并发**：单 session 是否允许多个并行 goal？建议初期限制为单 goal，避免状态机复杂化。
5. **LLM checker 的 JSON 健壮性**：模型可能返回带围栏或额外文字的输出，需稳健解析与失败降级策略。
6. **与 subagent 架构的关系**：goal 推进中 Worker 是否可派生 subagent？两者的预算如何归并计量。

---

## 10. 分期建议（增量交付）

**Phase 1 范围切分（重点）**——明确哪些不被 auto mode 阻塞、可现在就建并测：

不被阻塞、可独立交付的 `/goal` 内核：
- `Engine.Pursue` 状态机 + 预算 + 无进展启发式（连续 K 轮无文件改动 / verify 输出不变 → blocked）。
- 裁判分离：`LLMChecker` 读 Evidence + 上轮 `LastReason` 反馈续推（worker 不自评、checker 不跑命令）。
- 持久化：`session.Metadata`（`sessions` 表加 `metadata` 列，顺手修 Metadata 不落盘的坑）。
- `agent.TurnResult.TokensUsed` 累加改动、Evidence 投影。
- 以上在单测里用**放行式 approver 替身**即可跑通——这是测试替身，不是发布 auto mode。

被 auto mode 阻塞的（`/goal` 的「去喝咖啡」UX）：
- REPL 里 worker 真正不打断地连写文件/跑命令；在 auto mode 落地前仍会卡在 `ConfirmApprover` 的 y/N。

- **Phase 2**：`LLMChecker` 模糊条件分支完善、准入判定、确定性退出码快路径（从事件读已落下的退出码，不重跑命令）。
- **Phase 3**：完整预算（tokens/wall-clock）、可观测性细化、Evidence 捕获方式与 compaction 的协调、subagent 预算归并。

> 建议先交付 Phase 1 内核并在真实「测试通过/build 绿」类任务上验证条件设计，再决定 Phase 2/3 的优先级——避免过早投入。

---

## 11. 决策记录（ADR）

参考 Claude Code 与 Codex 已验证的形态，以下决定已锁定，作为落骨架的契约依据。

1. **裁判分离（不变量，抄 Claude Code）**：worker 永不自评；checker 只读 Evidence、自己绝不跑命令；命令一律由 worker 走其自有 sandbox/审批/cwd 路径执行。此不变量同时消除「checker 裸 exec 绕过 sandbox」的问题。
2. **持久化走 `metadata` 列（抄 Codex thread-state）**：goal 是单一当前态（gauge），存 `sessions.metadata` 列、last-write-wins，作为 resume 的唯一真相源；不做事件溯源。代价：需要一次 schema migration（顺带修 Metadata 不落盘）。终态审计若需要，仅在 achieved/blocked/cleared 额外发一条 `goal_state` 事件归档（additive）。
3. **auto mode = `/goal` 上游前置**：`/goal` 内核独立交付；hands-off 无人值守依赖 auto mode 先落地。两者是依赖关系，不是同一 PR。
4. **Evidence 源 = `sess.Messages` 的 `RoleTool` 全文**：用 `ToolCallID→toolName` 索引，白名单含 `run_command`/test/`git diff`；取全文而非一行摘要（摘要有损，是 worker 自我叙述渗入之处）。契约 = 「最近的、白名单内的工具结果，每条尾部截断」。Phase 1 在 check 时 read-live，Phase 3 再决定是否迁到 emit-时快照。
5. **token 双线**：`Spend.Tokens` = counter（agent loop 内逐次累加 `resp.Usage`）；compaction = gauge（`sess.PromptTokens`）。共享来源、不共享累加器。
6. **LLM checker 消耗计入预算**：概念上算 goal 成本；Phase 1 因独立模型 telemetry 未接线，先按 worker token 计、把 checker 漏计标为已知欠账（CC 刻意让评判模型小而便宜，占比是零头），telemetry 接通后补上。
7. **命名**：编排器用 `Engine`/`Pursuer`，避免与 `agent.Runner` 撞名；复用/适配 `agent.TurnResult`。

待 Phase 3 再拍的开放项：Evidence 捕获方式（read-live vs emit-快照）、subagent 与 goal 的预算归并、慢测试套件下的 check 频率调参。
