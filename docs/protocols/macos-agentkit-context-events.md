# macOS AgentKit: Context Management & Verification Event Display

> 目标受众：macOS AgentKit 前端开发。本文档描述 4 个新事件类型的 wire 契约和 UI 展示建议。
>
> 目前 macOS 端日志出现 `⚠️ [Wire] dropped unknown event kind=context_edited/pruned/pre_mutation` 警告，server 侧已完整支持这些事件，需要客户端补齐展示。

---

## 总体原则

这些事件分为两类：
- **上下文管理三元组**（context_edited → context_pruned → compacted）：三轮上下文的"发生了什么"可视化，让用户感知到上下文压力的来源和系统的应对策略
- **验证事件**（pre_mutation + verified）：P4.3-R 自检闭环，展示模型在编辑前的自我质疑和编辑后的确定性验证

**先展示，后优化**：当前目标是让这些事件在 UI 中可见，用户和开发者才能在真实使用中发现问题，再针对性优化 code-agent 的上下文策略。

---

## 1. context_edited — 上下文预清理

### Wire 契约

```json
{
  "kind": "context_edited",
  "at": "2026-06-24T10:00:00.123Z",
  "session_id": "sess_root",
  "turn_id": "turn_7",
  "text": "cleared 5 stale tool results"
}
```

### 语义

- **触发时机**：上下文压缩流程的第一步（LLM Compactor 运行之前）。ContextEditor 扫描旧轮次中的 tool 结果，将低信号内容（构建输出、目录列表等）替换为结构化骨架 `[cleared: bash go build ... (8.3KB)]`，保留高价值内容（代码片段、API 签名、用户决策）。
- **成本**：免费（零 LLM 调用）。
- **text 格式**：`cleared N stale tool results`，N 是被替换的 tool 结果数量。可能为 0（此时不发事件）。

### UI 展示建议

- **样式**：紧凑内联提示，比 `compacted` 更轻量。
- **图标**：🧹 或类似清理/过滤图标。
- **内容**：直接展示 `text` 字段，例如「🧹 清理了 5 条过期工具结果」。
- **折叠**：与后续的 `context_pruned` 和 `compacted` 形成上下文管理三元组时，可折叠到同一个"上下文压缩"卡片中。

### 示例交互

无需交互——纯信息展示。

---

## 2. context_pruned — 上下文截断（Tier-0）

### Wire 契约

```json
{
  "kind": "context_pruned",
  "at": "2026-06-24T10:00:00.123Z",
  "session_id": "sess_root",
  "turn_id": "turn_7",
  "before_tokens": 90000,
  "saved_tokens": 10000
}
```

### 语义

- **触发时机**：`context_edited` 之后、`compacted` 之前。PruneOldContext 对最旧的 tool 结果做硬截断（超过阈值的部分直接裁剪），并清理模型 think 块。
- **成本**：免费（零 LLM 调用）。
- **`before_tokens`**：截断前的上下文 token 数。
- **`saved_tokens`**：通过截断节省的估算 token 数（由字符长度估算，非精确 tokenizer 计数）。

### UI 展示建议

- **样式**：内联压缩卡片，与 `compacted` 区分（compacted = LLM 付费摘要，context_pruned = 免费截断）。
- **图标**：✂️ 或类似裁剪图标。不要复用 `compacted` 的图标——用户需要一眼区分"免费截断"和"付费摘要"。
- **内容**：展示前后的 token 量和节省量。例如「✂️ 截断上下文 · 10000 tokens 节省 · 从 90000 → 80000」。
- **与 compacted 的关系**：
  - `context_pruned` 和 `compacted` 的 `before_tokens` 指向**不同的**上下文快照——`context_pruned.before_tokens` 是截断前，`compacted.before_tokens` 是 LLM 摘要前（截断后的状态）。
  - 两者在同一次压缩流程中会依次出现，UI 不应将它们的 `before_tokens` 等同。

### 示例交互

无需交互——纯信息展示。

---

## 3. compacted — 上下文压缩（LLM 摘要）

### 已有事件，新增字段

```json
{
  "kind": "compacted",
  "at": "2026-06-24T10:00:00.123Z",
  "session_id": "sess_root",
  "turn_id": "turn_7",
  "before_tokens": 80000,
  "after_tokens": 30000,
  "saved_tokens": 50000,
  "summary_chars": 1200,
  "ratio": 0.37,
  "ineffective": false
}
```

**新增字段 `ineffective`**（boolean, omitempty）：
- `false`（或不出现）：摘要成功节省了 token。
- `true`：LLM 摘要没有有效减少上下文（节省比低于阈值），标记为无效压缩。

### UI 展示建议

- 当 `ineffective: true` 时，用不同颜色/样式展示（如灰色或黄色警告），提示用户此次压缩无效。
- 其他展示逻辑不变。

---

## 4. pre_mutation — 编辑前自检

### Wire 契约

```json
{
  "kind": "pre_mutation",
  "at": "2026-06-24T10:00:00.123Z",
  "session_id": "sess_root",
  "turn_id": "turn_7",
  "text": "Verify your hypothesis before editing"
}
```

### 语义

- **触发时机**：当模型决定执行编辑工具（write_file / edit），且本轮已经遇到过工具失败时，Agent 在真正执行编辑前会触发一次自检提醒——要求模型确认它理解了失败原因，再动代码。
- **这是 P4.3-R Move 3**：在"发现失败 → 分析原因 → 编辑修复"的闭环中，`pre_mutation` 标记了"分析完成，即将动手"的转折点。
- **不影响控制流**：这是一个观测事件，不是审批——编辑仍然会执行。

### UI 展示建议

- **样式**：thinking 块风格——这是模型的内部自检，类似 reasoning。
- **图标**：💭 或 🔍，与 reasoning/thinking 保持视觉一致性。
- **内容**：展示 `text`。例如「💭 编辑前自检：Verify your hypothesis before editing」。
- **位置**：在编辑工具卡（write_file/edit）**之前**展示，作为工具执行前的思维标注。

### 示例交互

无需交互——纯信息展示。

---

## 5. verified — 确定性验证结果

### Wire 契约

```json
{
  "kind": "verified",
  "at": "2026-06-24T10:00:00.123Z",
  "session_id": "sess_root",
  "turn_id": "turn_7",
  "text": "go build ./... passed, go test ./... all passed"
}
```

### 语义

- **触发时机**：Agent 完成答复后，finalize 步骤运行 `go build && go test`（或用户配置的验证命令），将实际编译/测试结果作为确定性验证。
- **这是 P4.3-R Move 2**：审核代码修改的是编译器和测试框架，不是 LLM。
- **text 内容**：验证命令和它的输出摘要。格式如 `"<command>: <result>"`。
- **结果类型**（嵌入在 text 中）：
  - 成功：`"go build ./... passed, go test ./... all passed"`
  - 失败：`"go build ./...: exit status 2\n./main.go:15: syntax error..."`
  - 无法运行：`"[verification could not run] <reason>"`

### UI 展示建议

- **样式**：代码块或终端输出风格卡片。成功时绿色/默认色，失败时红色。
- **图标**：✅ 成功 / ❌ 失败 / ⚠️ 无法运行。
- **内容**：展示完整 `text`。对多行输出（编译错误），使用可展开的代码块。
- **位置**：在 `turn_finished` 之前或与之关联展示——这是 turn 的收尾验证。
- **文件引用**：如果 text 中包含 `file:line` 引用（如 `./main.go:15: syntax error`），可做成可点击链接。

### 示例交互

- 成功：折叠或自动收起（用户不需要盯着绿色的成功结果）。
- 失败：保持展开，让用户看到具体错误。文件引用可点击跳转。

---

## 6. 事件时序关系（同一轮上下文压缩）

当触发上下文压缩时，事件的发送顺序为：

```
context_edited → context_pruned → compacted
   (免费)          (免费)          (付费 LLM)
```

macOS 端的建议处理：

- **合并展示**：三个事件可在 UI 中折叠为一个"上下文压缩"卡片组，展开后显示三步的细节。
- **独立展示**：如果只发生其中一步或两步（如 `context_edited` 返回 0 则不发射 `context_edited`；`context_pruned` 节省足够则跳过 `compacted`），UI 应各自单独展示，不强依赖三步齐全。

---

## 7. 前向兼容

所有新事件遵守 Wire Protocol v1 的"只增不改"原则：
- 客户端必须忽略未知字段（如未来可能新增的字段）。
- 未知 `kind` 应 no-op，不得 fatal。
- 这些事件都是 **fire-and-forget events**（server → client），不需要客户端回复。
