# 作业：把责任从 Human 脑子里搬出来 ——《从 Multi-Agent 到 Agent Team》精读

> 原文：《从 Multi-Agent 到 Agent Team》最佳实践（作者 yan5xu / 言午，CodexLoom）
> 来源：X 原文帖（https://x.com/yan5xu/status/2083004612207051208 ）、微信公众号原文（https://mp.weixin.qq.com/s/ZloR4kbXacxpcEkIEv3oUQ ）
> 说明：公众号正文抓取在 03 章后半起被截断，04–06 章内容交叉核对了作者的 GitHub 权威文档（`docs/topics.md`、`docs/owner-guide.zh-CN.md`、README）。

---

## 零、先交代这是什么

作者 yan5xu（言午）用一支自己跑了几个月的真实 Agent Team 写成的产品长文。表面上是 CodexLoom 的产品说明书，实际上是一份"多个 Agent 如何变成一支 Team"的组织学实践报告。全文 7 章是一条完整的论证链：**责任线如何从 Human 一个人身上，逐步外化给整支 Team 共享**。

一句话总结全文：**多个 Agent 不会自动成为 Agent Team。真正的分水岭不是 Agent 数量，而是"谁负责什么、遇到问题找谁、结果交给谁、现在是什么版本"这些原本只存在于 Human 脑子里的东西，是否变成了整个 Team 可以查询、使用和修正的工作结构。**

---

## 一、主线：一条连续责任线的五次迁移

全文机制示意图是一条线：**Task → Long-running → Domain Agents → Human Router → Agent Team**。每次迁移都不是"多开几个 Agent"，而是被真实工作逼出来的：

| 阶段 | 驱动因素 | 解决/暴露的问题 |
|---|---|---|
| Task Agent | 一次性、边界清楚的工作 | 无 |
| Long-running Agent | 真实工作是持续责任线，不是任务切片 | 冷启动的真正成本不是 token，是**每次重新建立合作关系** |
| Domain 分化 | Scope 扩张、不同 Context/判断互相干扰、能力劣化 | 单个 Agent 过载 |
| Human Router（瓶颈） | 多个 Agent 各自分工，但协作仍发生在 Human 脑子里 | 多个 Agent 并行，Human 只能串行 |
| Agent Team | 责任、关系、交接方式从 Human 外化 | Human 从唯一 Router 上移为 Owner |

文章的"楔子"用一次真实事故点题：作者本打算只上线 Landing Page，结果负责 Web 的 Agent 按既有协作关系通知了 Community Agent，后者直接把消息发进了飞书群。作者事后说"它们干得不错"——因为**工作沿着责任、协作关系和授权边界自己走完了，没有人站在中间当总线**。这正是整篇文的理想形态。

---

## 二、逐章拆解

### 01 一个 Agent，是怎么变成多个 Agent 的

**Task Agent 并没有问题。** 一次性工作用一次 Thread 很自然。问题在于真实工作大多不是彼此独立的切片——一篇文章写完还要改、一个页面上了线还要迭代、研究过的公司下个月还会再回来。而且每次回来都不是重复：上一次的纠正、偏好、边界必须继续生效。

反复冷启动的深层成本被作者点得很准：**不是 Token，不是多写几个 Prompt，而是每次都在重新建立合作关系。** Long-running Agent 的意义不是把对话拉长，而是"同一类责任开始有了一个持续存在的主体"。

**Long-running Agent 也有上限。** 一个 Agent 越来越好用 → 人不断加活 → Scope 先扩张、后劣化。作者给了一个反直觉的结论：**Domain 不是先画出来的领域标签，而是在 Scope 扩张和能力劣化中逐渐显现的工作边界。** 正确顺序是"真实工作暴露边界 → 边界被记录 → 继续验证修正"，而不是"先设计组织图再填人"。

**瓶颈转移到 Human。** 分化后单个 Agent 压力下降，但"入口、Context、结果、下一步"全部汇聚回 Human：Agent 可以并行，Human 只能逐个阅读、逐个判断、逐个路由。机制图上画得很形象：多条并行 lane 汇成 Human 面前一条 Context queue，再从一个窄出口串行流出。**Agent 越多，Human 要维护的 Context 和协作关系越多——"Agent 更多了，人却更忙了"。**

核心判断（原文引用）：*单个 Agent 过载推动 Domain 分化；Human Routing 过载推动 Multiple Agents 继续向 Agent Team 演化。*

### 02 先让"谁负责什么"离开 Human 的脑子

多个 Agent 出现后，作者发现新问题不是没有分工，而是**分工只存在于自己的脑子里**——每个 Agent 知道自己正在做什么，但不会因此知道 Team 里还有谁、别人负责什么、边界之外该找谁。Human 不仅是唯一 Router，还是唯一保存分工的 Directory。

解法是三层声明结构：

- **Profile**：回答三个问题——Identity（它是谁）、Domain（长期接住什么）、Scope（在哪里停下）。作者特意强调：Profile 不是工牌，也不是更长的 System Prompt，而是**把 Human 已经从真实工作中发现的边界记录下来**。它是"当前采用的组织假设"，不是能力证明，也不是最终答案。
- **Organization**：parent/child 的长期责任边界。
- **Collaboration**：两个独立 Domain 之间有方向的长期协作接口。

贯穿全文的一个核心纪律：**声明与证据分离**。Profile/Organization/Collaboration 是"声明"（Team 当前采用的组织假设），Activity 是从真实 Message 聚合出的"运行证据"，二者不能互相替代。声明不会自动授予权限、不会自动路由消息、不会因为互发了几条消息就自动升级成长期 Collaboration。作者反复强调：**真正值得沉淀的 Collaboration，要先在多次真实工作中表现为稳定的输入、输出和升级条件，再由 Human 明确写下并继续验证。**

本章的分水岭问题（原文关键问题）：*当工作需要协作时，当前 Agent 仍然只能回头问 Human"我应该找谁"，还是它能根据自己的 Scope、直接关系和主动查询到的 Profile，判断下一位候选 Domain Owner？* —— 查询只提供责任声明和关系依据，具体找谁仍是 Agent 的判断；边界不清继续升级给 Human。但如果每一步都要 Human 指定，那这些 Profile 就只是一张给人看的组织图。

### 03 让 Agent 自己开始协作

判断出"该找谁"只解决一半问题。另一半是**把工作直接交出去**：Message 进入接收方自己的长期 Thread，由接收方带着自己的 Profile、直接关系和已积累的专业 Context 来理解和处理，发送方的完整历史不会复制过去。Human 脑中那条链路（发现越界 → 找更合适的 Agent → 解释为什么找它 → 转交必要 Context → 等待处理 → 带回结果）被整体搬到了 Agent 之间。

**沟通意图被显式区分**：

- **Request**（`--response required`）：需要对方返回判断/行动/结果，回复沿原 Message 返回，保留真实因果关系；
- **Notification**（`--response none`）：只同步一个对方必须知道的状态变化，不需要为了"收到"制造无业务价值的确认消息。

Landing 外发案例里有全文最细的一个设计点：Web Agent 发给 Community Agent 的是一条 **notification**（告知已发生状态变化），Community 完成后没有把结果伪装成原消息的 reply，而是沿同一项工作重新发了一条 completion notification。因为这次协作的含义不是"Web 在等 Community 回答一个问题"，而是"状态已变化，Community 独立承担后续判断，再明确通知结果"。**消息类型表达的是协作关系，不只是通信效率。**

**为什么 Agent 沟通不是一次把一切说完**：多轮不是把一条完整消息拆碎，而是每轮只推进一个判断层（Open → Correct → Align → Converge）。作者的假设是：过长且过度预设的一次性 Prompt 容易强化发送方的错误 framing；让接收方先用自己的 Domain 上下文生成整体理解，等于为下一轮提供一份自提示的上下文支架，错误前提也更早暴露。当然作者诚实标注了这是假设——对成熟的稳定接口，一次性结构化请求仍然更高效。

### 04 Message 负责沟通，Topic 负责收口

当工作跨越多个 Agent、多个 Turn、多天之后，沟通解决不了"唯一当前版本"的问题——谁负责、在等什么、现在到哪、证据在哪。**Topic（中文叫"事项"）就是为此设计的薄共享协调记录**，全文我认为设计最扎实的一章。

边界定义非常干净：**Topic 不执行工作**，专业过程继续发生在每个 Agent 自己的 Codex Thread；Topic 只保存跨域所需的当前 Brief、责任、等待条件、关键证据和因果活动。它没有自己的 Thread、Turn、busy 状态。

四个核心概念各司其职：

- **Responsible（唯一）**：维护 purpose、completion boundary、带版本号的 brief、waiting_on、参与关系和最终收口。只有 Responsible 发布的阶段结果进入 Owner 的 Results Ready。
- **Participant（多个）**：只承担明确的 topic-scoped responsibility，在自己的 Thread 工作，把结果、限制和上下文缺口返回 Responsible。
- **Artifact**：最终文件的受管交接（`loom topic link ... artifact art_xxx`）。
- **Needs You**：只有工作在缺少 Owner 的某个事实、选择或授权时无法继续，才触发；要说明被什么阻塞、确切问题、每个选项的后果；Owner 回应后原 Agent 继续同一项工作，而不是要求重新陈述上下文。

几个特别值得注意的克制设计：

1. **版本化 Brief 防静默覆盖**：`--if-version 4` 乐观并发，多个恢复 Turn 不会无声覆盖彼此的结论；
2. **`resolved` 不自动结束任何东西**：不结束 Goal、不取消 Trigger、不关闭 Message、不 interrupt 任何 Turn——协调记录状态和实际执行是两回事；
3. **Results Ready 是注意力过滤器**：普通 Participant 输出不直接进 Owner 视野，避免"内部协作量变成 Owner 注意力负担"——这是全文反复出现的主题：**Human 的注意力是这支 Team 最稀缺的资源**；
4. **Owner 干预只作用于精确的 active Turn**（steer/interrupt），记录审计事件、通知 Responsible，但不自动改 Topic 状态、不自动重新分派——底层控制对象仍是 Agent Session 里的 active Turn。

与 Goal/Message/Schedule/Trigger 的区分也值得记：Goal 保留单个 Agent 长程运行的续跑与完成状态；Message 传递请求/回复/投递与因果；Needs You 保留一次持久的人类决定；Schedule 按时间唤醒；Trigger 是"去重新核验 Provider 状态的理由，而不是结论"——事件唤醒后仍要 Agent 回源核验，事实满足真实条件后 Responsible 才清除 waiting。

### 05 Overview：让一支持续变化的 Agent Team 变得可治理

当 Human 不再阅读每个 Agent 的全部过程，就需要一个更高层的观察面。本章的核心立场：**治理关注的是工作流动，不是让每个 Agent 看起来都很忙。**

Overview 提供的是诊断证据而非绩效排名：Status（什么在运行/等待/停止/不可用）、Inbox 与排队等待（可能反映压力、路由错误、资源占用、Connector 延迟）、Capacity（执行与等待的证据）、Token 用量（报告消耗，不报告业务价值）。作者反复提醒信号要组合解读：**高等待 + 低执行，可能说明 Goal、重启、权限或 Connector 问题，而不是产能不足**——这是全文最有工程素养的一句话。

"四类团队证据保持分离"是治理的骨架：

- Profile：一个 Agent **当前声明**拥有什么；
- Organization：长期上下级责任边界（声明）；
- Collaboration：独立领域间稳定横向接口（声明）；
- Activity：限定时间内**真实发生过**的证据。

Message 可以记录一次协作，但不因此变成 Organization/Collaboration；Topic 可以临时集合一组 Participants，而不声明长期关系。**声明结构不自动授予任何权限，也不强制消息路由。**

作者还提出了组织方法论上的"矩阵组织"（明确标注为组织方法，不是硬编码的 Agent 类型）：**Business Home**（稳定业务归属，长期拥有业务对象与跨 Topic 优先级）、**Topic Team**（围绕一件阶段性事项的动态组合，Responsible 不因此成为 Participants 的长期上级）、**Practice Coach Network**（跨 Business Home 维护专业方法，沉淀为 Skill/SOP/工具/模板）。同一个 Agent 可以属于一个 Business Home、参与多个 Topic、接受多个 Coach 的方法支持——这组设计把"责任"和"方法"解耦了，很有现实组织学的味道。

信号反复出现时的调查顺序（作者给了明确的操作步骤）：确定受影响的工作和稳定证据 ID → 询问负责的 Agent 它如何理解边界 → 只向相邻负责人核对 → 把任务/工具/调度/Connector 故障与组织设计问题分开 → **Owner 确认是长期责任变化后才修改 Profile/关系** → 观察之后的真实工作判断调整是否有效。证据保全和决定权归属始终优先。

### 06 Agent Team 怎样进入真实的外部关系

本章的关键命题：**把 Agent 接入 Slack、飞书远远不够。** 接进去只是给了它一个说话的管道；对外之后，身份、角色、信任、权限和现实后果必须分别治理。

架构是：`Feishu/Slack/Parall ↔ Interface Agent ↔ Domain Agent Team`。一条完整链路：Provider event → Connection/Address/Membership → Inbox/Handling → Interface Agent primary Thread → 可选内部 Agent 协作 → Outbox → provider result/receipt。

核心概念是 **Conversation Membership**：同一个 Agent 可以在多个平台拥有身份、参与多个群聊，而 Membership 定义它在**具体某个对话中的角色**——关注什么、何时说话、什么不可披露、何时必须咨询内部 Owner。所以同一个 Agent 能在不同 channel 扮演不同角色，而不会被复制成多个断连的 Agent。

几个关键边界：

- **Interface Agent 是组织模式，不是硬编码的 Agent 类型，也不是自动网关**：只有当外部关系上下文与判断值得一份自己的长期责任时才使用；否则一个 Domain Agent 可以直接持有受治理的外部角色；
- **外部 Membership 不授予外部 actor 对内部 Agent、Thread、工具、凭证或决策权的直接访问**——内部路由和披露仍受显式授权与信息边界约束；
- "让能力被复用，不等于把 Owner 的全部上下文、权限或业务判断交给外部协作者"——复用的是专业能力，不是授权。

这与现实中把 Agent 挂到群里就撒手的做法形成鲜明对照：**对外意味着要回答"它以什么身份、在什么对话里、能说什么、不能说什么、何时必须请示人"——每一层都是独立的治理对象。**

### 07 CodexLoom 在织什么

收尾章回答三个问题。

**Multiple Agents 与 Agent Team 的真正分水岭**：不是有没有消息系统，而是——原本全部集中在 Human 脑子里的责任、关系、交接方式、当前状态和边界，是否已经变成整支 Team 可以使用的工作结构。如果每一项工作仍由 Human 选择入口、整理背景、搬运 Context，那再多 Agent 也只是一组独立工具。

**Human 在 Team 中的新位置**：没有退出，只是不用再串联每一步，而是回到方向、事实、选择、Review 和授权的位置。"从唯一的 Router，逐渐上移为整支 Agent Team 的 Owner。" 注意作者用"上移"而不是"退位"——治理责任反而变重了。

**为什么需要稳定的 Agent 和动态的 Team**：Agent 是长期存在、Domain 稳定的主体（Thread 保留自己的历史和方向）；Team 是随真实工作动态调整的结构（Organization/Collaboration 会被修正，Topic Team 临时组建、试运行、证据稳定后才固化为长期声明）。Loom 的隐喻很准：**Codex 提供 Threads（线），CodexLoom 把它们织成组织（布）**——每条线保留自己的方向，织法可以改。

---

## 三、全文三个最值得带走的设计

1. **声明与证据分离（profile/organization/collaboration vs activity）**。这是全文的方法论基石：组织假设可以随时写、随时改，但必须由真实运行证据来检验；画一条线不等于授予权限，发过几条消息不等于一段长期关系。它防止了 Agent 组织图变成"看起来负责"的摆设。

2. **Topic 的"薄收口"哲学**。跨 Agent 的共享状态只保存最小必要信息（brief、责任、等待、证据链接），专业过程留在各自 Thread。`resolved` 不自动结束任何执行、版本号防静默覆盖、干预只作用于精确 active Turn——每一处都在对抗"协调层悄悄变成第二真相源"的熵增。

3. **Human 注意力经济学**。从 Results Ready 只收 Responsible 的阶段结果、notification 不制造无价值确认、到"信息流向责任，决定流向权限，结果回到发起者，例外向上升级"——整支 Team 的设计都在保护一个事实：**Human 依然只能串行阅读和判断，所以系统必须保证出现在 Human 面前的东西都是真正需要人的。**

---

## 四、批判性思考（作业里应该有自己的观点）

- **强项**：这套设计把"责任外化"做成了分层可操作的结构（声明/消息/收口/治理/外部边界），且每一步都保留了 Human 的决定权和回退路径；对 LLM 能力的边界（Context 有限、不可靠记忆）有清醒认识，用"组织假设 + 证据检验"代替"一次性把组织设计完美"。
- **疑问 1：声明的真实性**。Profile 是声明不是能力证明——如果 Agent 实际能力和 Profile 不符，谁在验证？作者用 Activity 和"真实工作检验"回答了一半，但"Agent 宣称负责某 Domain 但实际上做得差"的检测仍主要靠 Human 抽查，这层治理在文档里是最薄的。
- **疑问 2：Topic 的无收口悖论**。Topic 刻意不做排期、依赖图、自动派单，是刻意的克制；但当一个 Topic 有多个 Participant、跨多天时，"Responsible 的 Brief 更新"本身依赖 Responsible 的自律和召回——系统没有强制闭环。作者承认这是第一版限制，但它是 Agent Team 规模化后最先被冲击的地方。
- **疑问 3：单 Owner 假设**。全文明确只服务"高级个人 Owner / One Person Company"。Business Home / Topic Team / Practice Coach 的矩阵结构明显在向多人组织演化，但多 Owner、多 Tenant 下的信任与冲突（两个 Human 对同一 Agent 的 Profile 有分歧怎么办）完全没有涉及——这是产品定位，也是设计盲区。
- **疑问 4：渐进式沟通的模型依赖**。"分多轮收敛"优于"一次说清"是作者的假设，解释是 LLM 逐 token 生成的 framing 问题——这个假设合理但未验证，且对成熟接口明确例外。它是沟通策略，不是通信协议，不能被产品化过度。

## 五、对工程实践的启发（简短）

如果把这套思路落回我们自己的 agent 运行时：**AGENTS.md、skills、memory 对应的是"让责任离开人脑"的第一步**（稳定的项目约定和长期记忆）；真正缺的是**显式的 Team 层**——Agent 之间的有界 Message（带请求/通知语义）、跨 Agent 的收口对象（Topic 式的薄共享状态）、以及把 Human 注意力当稀缺资源的升级路径（只把真正需要人的决定送回来）。**"多个 Agent 并行但 Human 串行"这个瓶颈，是所有多 Agent 系统迟早要面对的结构性问题，值得在架构上提前留出外化接口。**

---

## 参考来源

- X 原文：https://x.com/yan5xu/status/2083004612207051208
- 微信原文：https://mp.weixin.qq.com/s/ZloR4kbXacxpcEkIEv3oUQ （03 章后半起抓取被截断）
- CodexLoom 文档（用于核对 04–06 章内容）：https://github.com/yan5xu/codexloom （docs/topics.md、docs/owner-guide.zh-CN.md、README）
- CodexLoom 官网：https://codexloom.ai

---

## 附录：从 Codex 原生能力看我们运行时的结构性差距

> 来源：@riba2534《Codex 进阶指南：作为 Multi-Agent 编排控制平面》（https://x.com/riba2534/status/2082916383248252976），约 2 万字，2026-07-30 发布。
> 性质：不是单纯的文档归纳——大量关于 wait-all 不存在的原因、fork_turns 的选择依据、outputSchema 在编排中的价值定位、Handoff 的两种语义混淆、Pipeline 的 barrier 反模式等内容不在 Codex 官方文档里，是高质量的实践报告。
> 说明：Codex 和 CodeAgent 在核心抽象上高度对齐（见下表），下面的差距不是"没有方向"，而是"方向对了但缺了关键原语或参数"。

### 已有对应物（先确认哪些我们已经做了）

| 文章概念 | code-agent 现状（已实现） |
|---|---|
| Task 内 Subagent（隔离上下文、只回结论） | `internal/tools/task/task.go` + `internal/runtime/subagent.go`（prompt 是唯一信道，父上下文零污染） |
| 执行环境 Worktree | `internal/managedworktree/` + `internal/runtime/managed_worktrees.go` |
| Graph/DAG 拓扑 | 自带 Flux DAG 引擎（`plan_workflow`、`internal/fluxstore/`），比 Codex 只能"自己写 App Server 客户端"更强 |
| Generator-Critic | harness gate（`task` 工具 `kind=change_review`，`VERDICT: PASS/REQUEST_CHANGES` 首行约束） |
| Hook 注入纪律 | `internal/hooks/`（目前是 pre/post-tool shell hook，非 subagent 生命周期 hook） |
| 事件流/可观测性 | `agent.Event` 40+ 事件类型 + `internal/trace/` + `internal/observation/` + jobs 事件分区 |
| 沙箱档位 | `internal/sandbox/` + `internal/approve/`（allow/confirm/block 三级 + AutoApprover） |
| 按角色选便宜模型 | `agent.subagent_model` 配置项（`internal/app/config.go:261`），子 Agent 可指定独立模型，不设则继承父 Turn |
| Goal/长任务 | `internal/goal/` + `/goal` 命令（headless、CI 兼容、exit code = outcome） |
| 多会话基础设施 | `internal/conversation/` + `internal/server/`（serve 模式、Agent Wire 协议、多 session 并发 turns——最近 commit 已支持同一 workspace 并发 turns） |

---

### 差距一：跨会话控制面——当前只能做 Task 内编排

这是 Codex 被称为「控制平面」的根本原因，也是最大的结构性缺口。Codex 的核心卖点是**模型自己能调用 `create_thread` / `wait_threads` / `fork_thread` / `handoff_thread` / `send_message_to_thread` 去编排其他会话**，而 code-agent 的编排方向目前仍是「人类 → 模型」。

**Codex 有什么而我们没有的**：

| 原语 | 用途 | 我们现状 |
|---|---|---|
| `list_threads` | 发现全 App 范围内所有会话及其状态 | `/sessions` 只能列本地 DB 历史，没有跨会话感知 |
| `create_thread` | 从当前会话创建新的持久 Task | 只能人工 `codeagent run`，Agent 不能建新会话 |
| `send_message_to_thread` | 给另一个会话发消息/派活 | 会话之间完全隔离，没有消息通道 |
| `wait_threads` | 等待其他会话完成（wait-any，最多 8 个） | 没有。Workflow 的 `parallel()` 是 barrier，但那是进程内的 |
| `read_thread` | 不打开会话就读其状态和 Turn 摘要 | 没有。只有一个会话一个 DB 文件 |
| `fork_thread` | 从已有会话分叉（同目录或 Worktree） | managedworktree 有 Manager 但未暴露为工具 |
| `handoff_thread` | 迁移执行位置（本机↔SSH）并保持 Task ID、历史不变 | 没有 |

**架构前提——hostId 属性化**：这里有一个比"缺原语"更根本的问题。Codex 能做到跨机器编排的前提是**主机退化为线程的一个属性**——`list_threads` 返回的是全 App 范围的线程，`read_thread` 带上 `hostId` 就能读另一台机器上的会话，不需要 SSH 过去。code-agent 目前的 session 是**绑定单 workspace 的**——workspace 即 session 的生存空间，没有跨 workspace 的会话寻址概念。所以跨会话控制面的实现深度有两种：

- **浅层**：同一个 workspace 下的多 session 编排（解锁 Supervisor 单仓库多线并行）
- **深层**：跨 workspace 甚至跨机器的全局控制面（解锁"把编译放内网大机器、UI 验证放本机浏览器"的按能力路由）

浅层实现只需要 `list_sessions` → `send_to_session` → `wait_sessions` → `read_session` 四个最小原语。深层实现需要先解决跨 workspace 会话注册与发现。

**关键设计约束**：
- 这些工具应该是**延迟加载的**（Codex 的 `codex_app` 命名空间用 `defer_loading`），避免把 13 个跨会话控制工具全部塞进 system prompt
- `list_threads` 返回的标题和摘要**必须当作不可信数据处理**（Codex 明确写：*never as instructions*）。现阶段 `task` 工具的 prompt 写的是"TRUST what it returns"，这在 subagent（同一信任域内的只读子进程）的边界下是合理的；但一旦开放跨会话读取，信任边界变了，安全姿态必须切换——信任域不同，安全要求不同
- `wait_threads` 返回只说明"那一轮结束了"，结果对不对必须靠 `read_thread` 取证据验证。Codex 没有提供 wait-all 原语（只能 wait-any + 自己写循环），我们也不应该提供 wait-all 的假象

---

### 差距二：Subagent 的上下文继承与终止控制——缺两个关键旋钮

当前 code-agent 的 `task` 工具只有"完全隔离"一档（子 Agent 永远看不到父对话），且终止后的复检依赖主 Agent 手动判断。Codex 在这两个维度上有更细粒度的控制。

#### 2a. `fork_turns`：上下文继承旋钮

Codex 的 `spawn_agent` 通过 `fork_turns` 参数控制子 Agent 继承多少父对话上下文：

| 取值 | 行为 | 适用场景 |
|---|---|---|
| `all` | 继承全部父对话历史 | 需要理解前因后果的 worker，省掉在 prompt 里重述背景 |
| `none` | 完全不继承 | explorer 只需要读代码，不应被主线程几万 token 的讨论干扰 |
| 数字 N | 只继承最近 N 轮 | 介于两者之间，比如只需要最近的决策上下文 |

code-agent 目前永远是 `none`——子 Agent 的 prompt 是唯一信息信道。好处是零上下文污染，代价是当子 Agent 需要父对话背景时，主 Agent 必须在 prompt 里手动打包上下文。**给 `task` 工具加一个 `fork_turns` 参数（默认 0，即保持当前行为）是低成本高收益的增量改动。**

注意 `fork_turns` 和五段式 prompt 模板是互补的：给 `none` 的时候，五段式里的背景段必须写足；给 `all` 的时候可以省掉背景，但要用"约束"段把注意力收窄，否则子 Agent 会顺着主线程的话题漂走。

#### 2b. SubagentStop 式自动复检循环

Codex 的 `SubagentStop` hook 可以返回 `decision: "block"` 阻止子 Agent 结束，让它继续跑一轮：

```json
{
  "decision": "block",
  "reason": "Run one more focused pass inside the subagent."
}
```

code-agent 目前的 `task` 工具 harness gate（`VERDICT: PASS/REQUEST_CHANGES`）是手动触发一次——子 Agent 返回 `REQUEST_CHANGES` 后，主 Agent 收到信号、判断要不要重新派。但"验证不过就让子 Agent 自己再跑一轮"的自动循环语义不存在。

**实现路径**：这个能力甚至不需要 Agent 类型系统作为前提。最简单的做法是在 `task` 工具层加一个 `max_attempts` 参数（默认 1，保持当前行为），配合 harness gate 的 VERDICT 判断——子 Agent 返回 `REQUEST_CHANGES` 且 `attempt < max_attempts` 时自动重跑，把上一轮的修订要求作为上下文注入。比 Codex 的 hook 方案更简单，不需要钩子基础设施。

---

### 差距三：Task 与 Subagent 的持久性区分——缺跨 workspace 寻址

Codex 的 Task/Subagent 区分有两层含义：

**第一层（生命周期）**：Task 是长期存在的、进侧边栏的、可以跨项目跨主机的持久 Agent Actor；Subagent 是 Task 内部临时派生的短生命周期工作者。两者的可见性和控制方式完全不同。

code-agent 目前没有这个区分——`task` 工具创建的子 Agent 是纯临时的（跑完即散），而持久 session 之间没有互相感知的通道。这意味着"每个项目有一个长期存在的 Owner Agent，Supervisor 向它派活、等它结果"这种拓扑做不了。

**第二层（寻址）**：Codex 的持久 Task 通过 `hostId` 在其所属主机上可寻址——`read_thread(threadId, hostId="remote-ssh-...")`。code-agent 的 session 绑定单 workspace，跨 workspace 没有寻址机制。

这两层合在一起，就是"全局 Supervisor / 跨项目交付"的前置条件。好消息是多 session 存储和并发 turns 的基础设施已在最近一次 commit 中到位（`internal/conversation/` 的 `TurnScheduler` 已支持同一 workspace 并发 turns），缺的是把 session 从"单 workspace 绑定"中松绑的第一步。

#### 跨 workspace 索引方案

当前存储架构的约束来自 `internal/runtime/store.go:127-141` 的 `storePath()`——它把 workspace 的绝对路径哈希成一个 DB 文件名：`~/.codeagent/projects/<basename>-<hash>/sessions.db`。每个 workspace 的 session 物理隔离在不同的 SQLite 文件中，没有跨 DB 的聚合查询能力。

最初这样设计是为了轻量化及可迁移（拷走/删除一个 workspace 的 DB 不影响其他），且这是参考 Claude Code 的架构——但 Claude 的做法并不是"只做隔离"，而是**存储隔离 + 索引共享**。

**Claude Code 的存储模型**（来自 `~/.claude/` 的实际文件结构）：

```
~/.claude/
├── history.jsonl          ← 全局轻量索引
│                            每行: { display, project, sessionId, timestamp }
│                            只存摘要，不存消息体
├── sessions/              ← 运行时元数据（按 PID 命名的小 JSON）
├── projects/              ← 按项目目录隔离
│   ├── -Users-...-projectA/     ← 目录名 = 绝对路径.replace("/", "-")
│   │   ├── <uuid>.jsonl         ← 一个会话 = 一个 JSONL 文件（重数据）
│   │   └── <uuid>/              ← 会话附属数据（subagents/, tool-results/）
│   └── -Users-...-projectB/
│       └── ...
```

**关键设计**：存储层按项目隔离（删一个目录不影响其他），索引层全局共享（`history.jsonl` 一个扁平文件就能回答"现在有哪些会话、各自在哪个项目"）。Claude 用 JSONL 做索引是因为他们没有 SQLite；code-agent 已经有 `internal/session/sqlite/` 基础设施，可以用一个更可靠的轻量 SQLite 来做。

**三种方案对比**：

| | 现状（per-workspace DB） | 方案 B（单一全局 DB） | 索引方案（Claude 模型） |
|---|---|---|---|
| 存储 | 一个 workspace → 一个 SQLite | 全局一个 SQLite | 不变：一个 workspace → 一个 SQLite |
| 隔离 | ✅ DB 文件级隔离 | ❌ 全混在一起 | ✅ 不变 |
| 可迁移 | ✅ 拷走一个 DB | ❌ 从大 DB 里筛选 | ✅ 不变 |
| 跨项目发现 | ❌ 没有 | ✅ 自动（`SELECT * FROM sessions`） | ✅ 查 `index.db` |
| 改动面 | — | `storePath()` 改一行 + 数据迁移 | 加一个 `index.db` + 写时同步 |

**推荐的索引方案**：

```
~/.codeagent/
├── index.db              ← 新增：全局轻量 SQLite（只存 Meta，不存消息体）
│   └── sessions 表
│       字段: session_id, workspace_path, name, model, status, updated_at
│       索引: (workspace_path), (updated_at), (status)
├── projects/             ← 已有：按 workspace hash 隔离
│   ├── projectA-<hash>/
│   │   └── sessions.db  ← 不变：存消息、事件、compaction 等重数据
│   └── projectB-<hash>/
│       └── sessions.db  ← 不变
```

**`index.db` 的语义**：
- **写**：`SessionStore.Save()` 成功后同步 upsert 一行到 `index.db`。只写 Meta 字段，不写消息体。失败不影响主存储（best-effort）
- **删**：`SessionStore.Delete()` 成功后删除对应行
- **读**：`list_sessions` 工具只查 `index.db`（零扫描开销），`read_session(id)` 先查 `index.db` 拿 `workspace_path`，再打开对应 `projects/<hash>/sessions.db` 读全文
- **一致性**：`index.db` 是派生数据，可以从已有 `projects/` 下的 DB 重建。启动时如果 `index.db` 为空或缺失，扫一遍已有 DB 重建索引

**不改的**：
- `storePath()` 的 workspace hash 逻辑不变
- `SessionStore` 接口不变
- 消息、事件、compaction 等重数据的读写不变
- 单个 workspace 的操作性能不变

**要改的**：
- `internal/runtime/store.go`：新增 `OpenIndex(path string) (*sql.DB, error)`，索引 DB 路径固定为 `~/.codeagent/index.db`
- `internal/session/sqlite/store.go`：`Save()`/`Delete()` 成功后同步写索引（best-effort，失败不阻塞主路径）
- 新增 `ListAllSessions()` 函数：直接查 `index.db`
- 工具层：`list_sessions` 调 `ListAllSessions()`，`read_session(id)` 先查索引拿 `workspace_path`，再 `OpenStore(workspacePath)` 读全文

**对比 Claude 的优劣**：Claude 的 `history.jsonl` 只有用户消息级别的粒度（一行一条 prompt），不追踪 session 状态。code-agent 已经有 session 级别的 `Meta` 结构（含 `TurnStatus`、`PausedAt`、`ArchivedAt`），用 SQLite 做索引比 JSONL 追加+解析更可靠，且天然支持状态过滤（"只列出 running 的 session"）。

#### Codex `state_5.sqlite` 与 code-agent `sessions` 表的结构对比

Codex 的实际情况是：**`state_5.sqlite` 就是 index.db**（SQLite 做索引），`sessions/YYYY/MM/*.jsonl` 才是重数据（JSONL 做对话记录）。code-agent 用 SQLite 同时承载了索引和重数据。以下是两边的 schema 级对比（均来自实际文件）：

**Codex `state_5.sqlite` — `threads` 表（34 列，170 行）**：

| 字段 | 类型 | 用途 |
|---|---|---|
| `id` | TEXT PK | Thread 唯一标识（UUID） |
| `title` / `name` | TEXT | 显示名 / 用户命名 |
| `cwd` | TEXT | 项目路径（对应我们的 `workspace_path`） |
| `model` / `model_provider` | TEXT | 模型和提供商 |
| `source` / `thread_source` | TEXT | 创建来源（user/cmd/fork/spawn） |
| `agent_nickname` / `agent_role` | TEXT | Agent 角色标识 |
| `sandbox_policy` / `approval_mode` | TEXT | 沙箱和审批配置 |
| `archived` / `is_pinned` | INTEGER | 归档/置顶状态 |
| `git_sha` / `git_branch` / `git_origin_url` | TEXT | 创建时的 Git 状态 |
| `tokens_used` | INTEGER | 已消耗 token |
| `created_at` / `updated_at` / `archived_at` | INTEGER / TEXT | 时间戳 |
| `rollout_path` | TEXT | 对话 JSONL 文件路径 |
| `first_user_message` / `preview` | TEXT | 首条消息 / 预览摘要 |
| `reasoning_effort` / `memory_mode` / `history_mode` | TEXT | 推理/记忆/历史模式 |
| `cli_version` | TEXT | 创建时的 CLI 版本 |

**Codex `state_5.sqlite` — 额外表**：

| 表 | 用途 |
|---|---|
| `thread_spawn_edges` | parent→child 的 spawn 关系（`parent_thread_id`, `child_thread_id`, `status`） |
| `thread_dynamic_tools` | 每个 thread 的动态工具配置 |

**code-agent `sessions` 表（17 列 + JSON blob）**：

| 字段 | 类型 | 用途 |
|---|---|---|
| `id` | TEXT PK | Session 唯一标识（时间戳+随机） |
| `name` | TEXT | 显示名 |
| `workspace_path` | TEXT | 项目绝对路径 |
| `workspace_root` / `_rel` / `_ext_id` | TEXT | 可移植 workspace 身份（iOS 场景） |
| `model` | TEXT | 模型 |
| `summary` | TEXT | compaction 累积摘要 |
| `prompt_tokens` / `context_window` / `compact_threshold` | INTEGER | Token 预算管理 |
| `created_at` / `updated_at` / `archived_at` | TEXT | 时间戳 |
| `metadata` | TEXT (JSON) | 可扩展元数据（含 `turn_status`, `paused_at`） |
| `gateway_assets` / `reference_ledger` | TEXT (JSON) | 资产和引用 |

**code-agent 关联表（消息和事件在同一 SQLite 中）**：

| 表 | 用途 |
|---|---|
| `messages` | 对话消息（`session_id`, `seq`, `role`, `content`, `tool_calls`） |
| `session_events` | 事件流（`session_id`, `turn_id`, `kind`, `payload`） |
| `compactions` | compaction 统计 |
| `requests` | API 请求遥测 |
| `managed_worktrees` | worktree 生命周期 |

**关键差距对照**：

| 能力 | Codex (`state_5.sqlite`) | code-agent (`sessions` 表) | 差距 |
|---|---|---|---|
| 跨项目列出所有 thread/session | `SELECT * FROM threads`（单表查询，170 行） | 需要扫多个 `projects/*/sessions.db` | **缺 SQLite 层索引** |
| spawn 关系追踪 | `thread_spawn_edges` 表（parent→child, status） | 无 | **缺整张表** |
| 创建来源 | `source` / `thread_source` 字段 | 无。无法区分 user 创建还是 Agent spawn | **缺字段** |
| Git 快照 | `git_sha`, `git_branch`, `git_origin_url` | 无。仅 managed_worktrees 表有 commit | **缺字段** |
| 置顶 | `is_pinned` | 无 | **缺字段** |
| Agent 角色 | `agent_nickname`, `agent_role` | 无 | **缺字段** |
| Token 消耗 | `tokens_used` | `prompt_tokens`（只有 prompt 侧） | 不完整 |
| 对话记录 | JSONL 文件（`sessions/YYYY/MM/`） | 同一 SQLite 的 `messages` 表 | 不同选择，各有优劣 |
| 沙箱/审批策略 | `sandbox_policy`, `approval_mode`（per-thread） | 全局配置 | 不同粒度 |
| 上下文预算 | 无 per-thread 字段 | `context_window`, `compact_threshold` | **code-agent 更精细** |
| compaction | 无 per-thread 统计 | `compactions` 表 + `summary` 字段 | **code-agent 更完善** |

**结论**：两边各有长短。code-agent 在 token 预算管理和 compaction 上更精细（这是自己实现 agent runtime 的好处）。Codex 在**跨线程组织**上更完整——`thread_spawn_edges`、`source`、`is_pinned`、`agent_role` 这些字段不是"多存了几个属性"，而是让"Agent 之间的生产关系"变得可查询。我们的 `index.db` 建表时，应该把这些字段作为一等公民，而不是塞进 `metadata` JSON blob 里。

---

### 差距四：Workflow 脚本化——expressiveness 不够

我们已经有 `plan_workflow`（Flux DAG）和 `Workflow` 工具（`pipeline`/`parallel`/`agent`），但 Codex 的 Workflow 脚本在这几个维度上走得更远。

**1. 结构化输出（`outputSchema`）**

这是推文里最被低估的能力。Codex 的 `agent()` 可以接受 JSON Schema，子 Agent 返回已验证的对象——不需要正则解析：

> "没有它，'等子任务返回结果然后按结果分支'这件事只能靠正则和祈祷；有了它，Worker 的返回就是一个能直接进状态机的数据结构。"

在 `plan_workflow` 里，这意味着 DAG 节点间的数据流可以从"把自然语言粘给下一节点"变成"传一个可校验的 struct"。

**2. Token 预算循环**

Codex 脚本可以按 token 预算动态扩展搜索深度，用户给 `+500k` 的指令可以直接转化为搜索轮数。比当前固定步数限制更灵活。

**3. Pipeline 的正确语义**

推文强调：Pipeline 不应该写成"一批全部翻译完再一起进校对"（barrier），而应该是"谁先做完谁先往下走"（wait-any + 每项独立状态推进）。当前 `pipeline()` 实现需要确认是否采用了非 barrier 语义。

**4. Loop-until-dry 模式**

对于未知大小的发现型任务，连续 K 轮没有新发现才停止，而不是固定"找 N 个"。避免漏掉长尾问题。

---

### 差距五：自动化触发（Heartbeat/Cron）——长任务运维缺周期唤醒

code-agent 有 `goal`/`auto` 模式但无周期触发。Codex 提供两种触发器：

- **Heartbeat**：挂在当前线程上主动跟进，保留原对话和目标，每次唤醒读上一次状态再往下推。适合"盯部署""盯 Incident"这类连续性任务。官方的默认选择。
- **Cron**：针对某个项目的独立作业，每次运行更接近全新 job。适合每日扫描、周期报告、依赖检查。

code-agent 的 `ScheduleWakeup` / `CronCreate` 工具已在工具列表中，但长任务运维（盯部署、每日扫描、跨天 Goal）的周期唤醒实践这块是空白。

---

### 优先级排序（按改动粒度，而非 feature 完整性）

```
P0 — 立即能做（0 依赖，纯 prompt/skill 层面）
├─ 五段式 Subagent prompt 模板固化为 skill
│   （目标/工作目录/范围/约束/返回格式+file:line 证据）
│   与现有 code-review skill 互补，"约束"段的并行去重纪律尤其实用
└─ 拓扑选型表固化为 skill 的 reference
    （8 种拓扑 + 各自适用场景 + 原语序列）

P1 — 增量改动（依赖 task 工具扩展，改动面小）
├─ task 工具加 fork_turns 参数（默认 0，保持当前行为）
├─ task 工具加 max_attempts 参数（默认 1，配合 VERDICT 自动重跑）
└─ SubagentStart hook 按 agent_type 注入 additionalContext

P2 — 架构演进（依赖 server 层 + managedworktree）
├─ 跨 workspace 会话寻址（session 从"绑定单 workspace"松绑）
├─ list_sessions → send_to_session → wait_sessions → read_session 四原语
└─ 自动化触发（Heartbeat/Cron）长任务运维实践

P3 — 长期方向
├─ outputSchema：DAG 节点间传结构化数据
├─ Workflow resume/cache：长 Workflow 的迭代调试
└─ 跨主机全局控制面（hostId 属性化 + SSH 路由）
```

关键判断：**P0 是认知工作（把推文里的五段式模板和拓扑选型表翻译成我们的 skill 格式），P1 是几十行代码的参数扩展，P2 才需要动架构。** P0 + P1 做完，code-agent 的 Subagent 编排能力就会接近 Codex 的 Task 内编排层；P2 做完，就是文章标题里那个「编排控制平面」。

### 与 CodexLoom 文章的关系

两篇推文从不同方向指向同一个问题。CodexLoom（言午）的文章告诉你**"应该织成什么"**——Profile、Organization、Collaboration、Topic、Message 这些组织学概念如何从 Human 脑子里外化。@riba2534 的文章告诉你**"Codex 这台织机原生提供了哪些线"**——跨会话控制原语、上下文继承旋钮、自动复检循环、Workflow 脚本引擎这些底层能力如何组合成拓扑。一个回答 what，一个回答 how。两份合在一起，就是我们 Agent Team 路线图的上半部分（组织结构）和下半部分（运行时原语）。
