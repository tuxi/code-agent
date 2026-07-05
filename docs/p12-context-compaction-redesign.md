# P12 — 上下文压缩重设计（收敛性、token 计价、分层降载）

> Status: **设计定稿，实施中。** 根因来自一次真实故障：本地小窗口模型触发压缩后
> 陷入"每轮都压、永远压不下去"的死循环。调研了 Claude Code / Gemini CLI /
> Codex CLI / OpenCode 的压缩实现后确认：**架构方向（摘要旧消息 + 保留近期消息）
> 没有错，但缺了主流实现里两个结构性部件** —— 保留窗按 token 计价、压缩失败护栏。
> 本文档先记录调研（教科书部分），再给出五点重设计（工程部分）。

## 1. 故障现场

本地模型（32k 窗口）跑 rust-practice 练习时的真实 trace：

```
⤳ context compacted — 42677→0 tokens (saved 0, summary 331 chars)
▸ Thought for 575s
⤳ context compacted — 42677→43849 tokens (saved -1172, summary 331 chars)
⤳ context compacted — 43849→0 tokens (saved 0, summary 473 chars)
▸ Thought for 711s
⤳ context compacted — 43849→44293 tokens (saved -444, summary 473 chars)
⤳ context compacted — 44293→0 tokens (saved 0, summary 637 chars)
... （无限循环，summary 331→473→637→1150→1531→1730 chars 缓慢爬升）
```

三个可观测事实：

1. 每轮循环压缩一次，`saved` 几乎恒为负 —— 压缩不但没省，反而变大。
2. summary 每次只长一点点 —— 每轮实际只 fold 掉一两条旧消息。
3. 每次压缩本身是一次完整的 summarize LLM 调用，本地模型一次要几分钟，
   痛感被放大到不可用。

同一设计在 DeepSeek（128k 窗口）上"压得很快"，一度让人误判为
"本地模型摘要能力太差"。**这是误诊** —— trace 里摘要一直在正常产出；
换一个完美的摘要模型也一样死循环。见 §3 根因。

## 2. 现状（P12 之前的实现）

- **何时压**：[budget.go](../internal/session/budget.go) `NeedCompaction()` =
  `PromptTokens >= CompactThreshold`；阈值 = `context_window × compact_ratio`
  （[config.go](../internal/app/config.go) `CompactThreshold()`，默认 ratio 0.5）。
  `compact_ratio` 的配置链路完整生效（run / repl / daemon / embed / subagent
  五个入口都消费它），但它**只决定何时触发，不参与压缩后留多少**。
- **怎么压**：[compactor.go](../internal/session/compactor.go) `LLMCompactor` ——
  累积摘要设计：旧消息 render 成文本，连同上一轮 Summary 一起交给模型
  产出新的累积摘要，历史重建为 `system → summary → 最近 N 条`。
- **留多少**：`KeepRecentMessages: 50`，硬编码在
  [runner.go](../internal/runtime/runner.go) `BuildCompactor`，**按消息条数计价**，
  与窗口大小、compact_ratio 零关联。
- **压缩后**：`PromptTokens` 故意不重置（[loop.go](../internal/agent/loop.go)
  `maybeCompact` 注释），等下一次模型调用的真实 usage 刷新并
  `FinalizeCompaction` —— 这个"实测而非假设"的设计本身是对的，
  但它把收敛责任完全押在"压缩必然有效"这个未被保证的前提上。

## 3. 根因分析

**致命缺陷 ①：保留窗按条数不按 token，收敛没有数学保证。**

压缩后的大小 = system prompt + summary + 最近 50 条消息。50 条消息的 token
数是无界的。在 128k 窗口下（阈值 64k），50 条通常远小于 64k，一次压缩就落回
阈值以下 —— 设计*碰巧*工作，这就是 DeepSeek "压得很快"的全部原因。在 32k
窗口下（阈值 16.4k），50 条消息 —— 还被本地 thinking 模型全量持久化的
reasoning（[types.go](../internal/model/types.go) `AssistantMessage()` 原样保存
`resp.Content`）撑肥到 ~40k —— 恒大于阈值，**数学上永不收敛**。每轮只有
滚出 50 条窗口的一两条旧消息被 fold（对应 summary 每次只长一点），同轮又新增
一条巨大的 assistant 消息，净节省为负。

**致命缺陷 ②：没有"压缩无效"的失败状态机。**

`FinalizeCompaction` 明明测出了 `saved ≤ 0` / `after ≥ threshold`，却没有任何
代码消费这个信号。`maybeCompact` 的注释甚至明写 *"A compaction that changed
nothing keeps NeedCompaction true, and the loop tries again"* —— 无限重试是
设计使然，只是没料到"永远无效"这种输入。每次徒劳的重试烧一次 summarize
调用。

**放大器（非根因但加重痛感）：**

- 本地 thinking 模型的 `<think>` 全文进历史，消息肥大速度数倍于远程模型。
- 触发点 0.5 偏早（主流 70–95%），小窗口下等于只留一半窗口可用。
- TUI 把 pending 压缩事件（`AfterTokens` 尚未实测，零值）渲染成
  `→0 tokens (saved 0)`，制造了"压缩成功但归零"的误读
  （[view.go](../cmd/codeagent/tui/view.go) `compactionLine`；
  [console.go](../cmd/codeagent/console.go) 反而处理对了）。

## 4. 主流 agent 压缩设计调研（教科书部分）

### 4.1 逐家拆解

**Claude Code —— 全量替换 + 分层降载**

- 触发：headroom 记账（为输出和压缩流程本身预留空间），约 92–95% 窗口。
- 压缩：结构化摘要（用户意图 / 关键技术决策 / 涉及文件 / 错误与修复 /
  待办与当前状态 / 下一步）**整体替换**全部历史；近期文件内容靠压缩后
  重新读取恢复，todo/plan 状态重建，boundary marker 标记压缩点。
- **Microcompaction**：在全量压缩之前，旧 tool 结果先落盘冷存
  （按路径可取回），近期结果保持 inline —— 确定性操作，零 LLM 成本。
- 收敛：结构性保证 —— 压缩后 ≈ system + summary，必然远小于窗口。

**Gemini CLI —— 摘要头部 + 保留尾部（token 计价）+ 失败状态机**

- 触发：70% 窗口（`COMPRESSION_TOKEN_THRESHOLD 0.7`）。
- 保留：最近 **30% token**（不是条数）；`findCompressSplitPoint()` 把切分点
  走到安全边界（user 消息处，绝不切开 tool call/response 组）。
- 摘要：结构化 XML `<state_snapshot>`（overall goal / key knowledge /
  file system state / current plan）。
- **失败处理：`COMPRESSION_FAILED_INFLATED_TOKEN_COUNT` 状态 +
  `hasFailedCompressionAttempt` 标志，压缩无效即停止重试。**
- 他们踩过与我们一模一样的坑：issue #16213（P0）"compression loop:
  每轮尝试压缩但 token 降不下来"，根因正是失败标志未生效，PR #16914 修复。
  **这证明失败护栏是此类设计的必要部件，不是锦上添花。**

**Codex CLI —— 摘要 + 保留最近 user 消息（token 硬上限）**

- 触发：按模型的 token 上限（`model_auto_compact_token_limit`），封顶 90%。
- 保留：最近的 user 消息，硬上限 **~20k token**；重压时显式剔除旧摘要，
  只让最新一份摘要存活（防摘要堆积）。
- 摘要：结构化 handoff（current progress / key decisions / constraints /
  remaining TODOs / critical data to continue）。
- 失败处理：指数退避重试。托管模型另有服务端 `/v1/responses/compact`。

**OpenCode —— 两级：先剪枝后摘要**

- 触发：`isOverflow()`，tokens > context_limit − output_limit。
- **Tier-0 剪枝**：40k token 保护窗之外、大于 20k 的旧 tool 输出直接替换为
  占位符 —— 零 LLM 成本，先回收大头。
- Tier-1 全量摘要（what was done / current work / files modified /
  next steps / key decisions with rationale）。

**Amp（Sourcegraph）**：只有手动 handoff，哲学上鼓励短会话 —— 不适用于
我们的长会话场景，仅作参照。

### 4.2 主流共性（一条不缺）

| # | 共性 | Claude Code | Gemini CLI | Codex | OpenCode |
|---|---|---|---|---|---|
| 1 | 保留窗按 **token** 计价 | ~0（全替换） | 30% token | 20k token | 40k 保护窗 |
| 2 | 压缩后大小**构造上有界** → 收敛是设计保证 | ✅ | ✅ | ✅ | ✅ |
| 3 | 无效压缩的显式处理 | —（结构上不会无效） | 失败状态机 | 指数退避 | —（结构上不会无效） |
| 4 | LLM 之前的确定性降载层 | microcompact 落盘 | — | — | tool 输出剪枝 |
| 5 | 结构化摘要契约（固定段落） | ✅ | ✅ state_snapshot | ✅ handoff | ✅ |
| 6 | 触发点 70–95%（给窗口留足工作空间） | ~92–95% | 70% | ≤90% | 输出预算制 |

**教科书结论**：压缩的正确性不能依赖摘要模型的能力，必须由*构造*保证 ——
"摘要（有界）+ token 计价的尾巴（有界）< 阈值"是不等式，不是期望。
摘要质量只影响*恢复效果*，不允许影响*收敛性*。我们的设计恰好把这两件事
搅在了一起：收敛性押在了"50 条恰好不太大"上。

## 5. 重设计（五点，全部采纳）

架构保持 Gemini/Codex 一系的"摘要头部 + 保留尾部 + 累积摘要"，不推翻。

### P12.a Tail 改 token 预算制（收敛的构造保证）

- `LLMCompactor.KeepRecentMessages`（条数）→ `KeepRecentTokens`（token 预算）。
- 预算 = `CompactThreshold × compact_keep_ratio`，新配置
  `agent.compact_keep_ratio`，默认 **0.3**（对齐 Gemini 的 30%），(0,1) 校验。
- token 近似：`len(content)/4`（含 tool call 的 name+arguments）。不追求精确 ——
  预算的作用是量级正确，实测仍由下一次调用的 usage 完成。
- 从尾部向前累计，超预算即为切分点；沿用既有"不切开 tool 组"回退逻辑；
  至少保留最后一条消息（极端小阈值下 fold 其余全部，收敛优先于语境完整，
  P12.b 兜底）。
- 收敛不等式：`system + summary + keep_ratio×threshold < threshold`
  （只要 system+summary < (1−keep_ratio)×threshold，正常配置下恒成立）。

### P12.b 压缩失败状态机（Gemini 教训的直接移植）

- `NeedCompaction()` 增加收敛护栏：最近一次已实测（finalized）的压缩若
  `AfterTokens ≥ CompactThreshold`（无效压缩），则进入冷却 ——
  直到 `PromptTokens ≥ AfterTokens + threshold/10` 才允许再压。
  状态从既有的 `Compactions` 观测日志**派生**，不新增持久化字段；
  重启后最多多试一次，护栏随即重新生效。
- loop 在 `FinalizeCompaction` 处发现无效压缩时，发一条显式 warning 事件
  （复用 `EventCompacted`，前端按 `saved ≤ 0 且 after ≥ threshold` 渲染警告），
  用户看到的是"压缩无效：上下文超出模型窗口"，而不是无声的循环。

### P12.c Tier-0 确定性剪枝（OpenCode/Claude Code 的廉价层）

- 新增 `internal/session/prune.go`：`maybeCompact` 里先于 LLM 摘要运行 ——
  - 保护窗之外（同 P12.a 的 token 预算边界）的 RoleTool 消息，
    内容超限的截断为「头部 200 字符 + `[pruned: N chars]` 占位」；
  - 保护窗之外的 assistant 消息剥离 `<think>…</think>` 块
    （主流一致：intermediate reasoning 是压缩最先丢弃的东西）。
- 若估算（`PromptTokens − savedChars/4`）已落回阈值下，**本轮跳过 LLM 摘要**，
  交给下一次调用实测 —— 对本地模型这省下的是分钟级的 summarize 调用。
- 发 `EventContextPruned` 事件（wire 需同步映射）。

### P12.d 结构化摘要契约

`summarizeSystemPrompt` 从自由文本改为固定段落契约（对齐
state_snapshot/handoff）：**Goal / Key knowledge & decisions / Files & state /
Errors & fixes / Plan & next step**。累积 fold 机制不变。弱模型在固定段落
约束下的产出质量显著好于自由发挥。

### P12.e 调参与显示修复

- `defaultCompactRatio` 0.5 → **0.75**（主流 70–95% 区间的保守取值，
  为输出和压缩流程留 25% headroom）；`config.example.yaml` 同步。
- TUI `compactionLine` 区分 pending（`After == 0`，渲染
  "measuring on next call"）与 finalized；无效压缩渲染为警告。
  console.go 已正确，timeline/transcript 对齐它。

## 6. 不做什么（non-goals）

- **不做落盘冷存/按路径取回**（Claude Code 的 microcompact 完整形态）——
  需要 asset 基建配合，Tier-0 截断已覆盖主要收益；留待后续。
- **不做压缩后文件重读恢复**（Claude Code 的 rehydration）—— 依赖
  "最近访问文件"追踪，另立项。
- **不换掉累积摘要**为"每次全量重摘要"—— 累积 fold 对增量压缩更省，
  且 Codex 的"剔除旧摘要"问题我们结构上不存在（Summary 单点存储，
  `Messages[1]` 恒为最新渲染，见 [session.go](../internal/session/session.go)
  Summary invariant）。

## 7. 验收标准

1. 32k 窗口 + 长会话：压缩一次后 `PromptTokens` 实测落回阈值下（收敛）。
2. 病态配置（阈值 < system+summary）：最多一次无效压缩即冷却，
   有显式警告事件，不再每轮烧 summarize。
3. 含超大 tool 输出的会话：Tier-0 剪枝独立完成降载，全程零 summarize 调用。
4. 既有测试 + 新增测试全绿（`compactor_test.go`、`compaction_stats_test.go`、
   `internal/agent/compaction_test.go`、config 校验）。

## 8. Sources

- [Decode Claude — Inside Claude Code's Compaction System](https://decodeclaude.com/claude-code-compaction/)
- [ClaudeLog — What is Claude Code auto-compact](https://claudelog.com/faqs/what-is-claude-code-auto-compact/)
- [Gemini CLI issue #16213 — Context compression loop（我们 bug 的同款，P0）](https://github.com/google-gemini/gemini-cli/issues/16213)
- [DeepWiki — Gemini CLI Chat Compression and Context Management](https://deepwiki.com/google-gemini/gemini-cli/4.12-chat-compression-and-context-management)
- [badlogic gist — Context Compaction Research: Claude Code, Codex CLI, OpenCode, Amp](https://gist.github.com/badlogic/cd2ef65b0697c4dbe2d13fbecb0a0a5f)
- [Codex Knowledge Base — Codex CLI Context Compaction Architecture](https://codex.danielvaughan.com/2026/03/31/codex-cli-context-compaction-architecture/)
- [Justin3go — Context Compaction in Codex, Claude Code, and OpenCode](https://justin3go.com/en/posts/2026/04/09-context-compaction-in-codex-claude-code-and-opencode)
