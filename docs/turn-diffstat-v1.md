# Turn 级 diffstat 需求（v1）

> 目标读者：code-agent 后端开发。目的：让客户端在一轮对话结束后显示
> **「用时 24分40秒 · 编辑 3 个文件 +1198 −24」**，服务端负责给出本轮的文件变更统计。
> 客户端（AgentKit / Web）已按本文契约留好接线位。

---

## 1. 背景

客户端已经完成「完成态折叠」：一轮结束后过程收起，头部只显示 `用时`（由 `turn_finished.at`
与 `turn_started.at` 相减得到）。现在缺的是「这一轮改了什么」，而客户端拿不到可信数据：

- `turn_finished` 现有字段只有 Text / tokens / 工具调用计数，没有文件维度；
- `apply_patch` 的输出是固定文案 `"Patch applied successfully."`，`edit_file` 输出是自定义编号
  diff（没有 `@@`），客户端解析会随文案变化静默失效；
- 子代理/工作流里的写入不进父流，客户端无论如何都统计不到。

**结论：必须服务端算。** 客户端只在折叠头追加一段文案，不重复计算。

## 2. 协议契约（字段名与语义以此为准）

`wire event` 顶层新增 `diffstat`，与 `executed_tool_calls` 同级（snake_case，`omitempty`）：

```json
{
  "kind": "turn_finished",
  "turn_id": "...",
  "text": "...",
  "diffstat": {
    "files": [
      { "path": "internal/agent/loop.go", "added": 12, "removed": 3, "status": "modified" },
      { "path": "docs/turn-diffstat-v1.md", "added": 96, "removed": 0, "status": "added" }
    ],
    "files_changed": 2,
    "added_lines": 108,
    "removed_lines": 3
  }
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `files[].path` | string | 是 | 工作区相对路径，`/` 分隔，不带 `./` 前缀 |
| `files[].added` | int | 是 | 本文件本轮新增行数，可为 0 |
| `files[].removed` | int | 是 | 本文件本轮删除行数，可为 0 |
| `files[].status` | string | 是 | `added` / `modified` / `deleted` |
| `files_changed` | int | 是 | 全量去重文件数；**不因 `files` 截断而变小**（未截断时等于 `len(files)`） |
| `added_lines` | int | 是 | 全量新增行数 |
| `removed_lines` | int | 是 | 全量删除行数 |

硬性要求：

1. **无变更时整个 `diffstat` 字段 omit**，不得下发空对象或全 0 对象。
2. 出现在所有 turn 终态事件：`turn_finished`、`turn_failed`、`turn_cancelled`
   （`turn_paused` 不带）。
3. **`files` 最多 200 条**，按 `added+removed` 降序截断；三个汇总值始终是全量真实值。
4. 历史兼容：已持久化的老事件没有该字段，原样返回即可，**不回填**。客户端把缺失视为“未知”，
   不会显示“编辑 0 个文件”。
5. 老客户端会忽略未知字段，因此**服务端可以先发**。

## 3. 计算口径（必须按这个来，否则数字对不上）

- **范围是本轮，不是工作区。** 只统计本 turn 内成功执行的写类调用，不能拿
  `git diff` 直接出结果——工作区可能带着用户自己的脏改动。
- **只统计成功。** 工具报错、`apply_patch` 返回 `Patch did not apply; no files were changed.`、
  被拒绝/取消的调用，一律不计。
- **同一文件多次写入按次累加**（= 本轮写入行数总和），**不做净变化**。
  理由：净变化需要在 turn 开始时对文件做快照，而工作区不保证是 git 仓库
  （见 `internal/tools/git/diff_gogit.go:32` 的 `not a git repository` 分支），成本与覆盖面都不划算。
  这个口径意味着数字可能**高于** `git diff --numstat`，属于已知且有意的偏差。
- **二进制文件**：计入 `files_changed`，`added/removed = 0`。
- **重命名**：v1 记为 `modified`，不额外计 `added`/`deleted`。
- **删除**：整文件删除**只有 `apply_patch` 会产生**（两个 backend 都支持，见
  `internal/tools/git/apply_patch_gogit.go:57` 的 `f.IsDelete`；`execApplier` 走 `git apply` 同理）。
  `edit_file` 的 `allow_delete: true` 删的是**一段文本**，文件本身保留（可能被删成空文件），
  状态必须记为 `modified`，不能记 `deleted`。
- **`run_command` 里的写操作不计**（`sed -i`、重定向、`mv` 等）。UI 文案限定为“编辑类工具”，
  验收以此为准。后续要用 git 快照方案才能覆盖，不在 v1。
- **`send_to_session` 派发的跨会话工作不计**。目标 session 有自己的 workspace 与 turn，
  其写入属于目标 turn 的 diffstat。v1 只覆盖本 turn 内、本 workspace 的写类调用。
  文件写入工具当前只有四个：`apply_patch`、`edit_file`、`insert_file`、`create_file`（已核对注册表）。

## 4. 实现落点（已核对现有代码）

### 4.1 数据结构

- `internal/agent/event.go`：新增

  ```go
  // tools 侧定义 FileWrite，agent 侧别名——tools 不能 import agent，
  // 直接在这里声明 DiffFile 会形成循环依赖。
  type DiffFile = tools.FileWrite

  type TurnDiffStat struct {
      Files        []DiffFile `json:"files"`
      FilesChanged int        `json:"files_changed"`
      AddedLines   int        `json:"added_lines"`
      RemovedLines int        `json:"removed_lines"`
  }
  ```

  `DiffFile` 的字段与 json tag 定义在 `internal/tools/diffstat.go` 的 `FileWrite`
  （`path` / `added` / `removed` / `status`），agent 用类型别名共用一份，避免两处 tag 漂移。

  给 `Event` 加 `DiffStat *TurnDiffStat`（`internal/agent/event.go:224`，紧接 `ExecutedToolCalls` 注释行，
  标明只在终态出现）。
- `internal/agent/loop.go`（`TurnResult`，394 行起，计数器在 421–423 行）加 `DiffStat *TurnDiffStat`。

### 4.2 累加点

`internal/agent/tool_batch.go` 的 `commitToolResult`（353 行）是**唯一**收敛点：每个执行的
调用都会在这里累加 `ExecutedToolCalls`（`:355`）。在此按 `p.call.Function.Name` 分派计数，
`res.executed == false` 直接跳过（被拒绝/未执行）。

**但 `res.succeeded` 不能当作「真的写成功」**，见 4.2.1。

#### 4.2.1 失败是「返回值」，不是 error（必须处理，否则数字会虚高）

这四个工具把**可恢复失败当作普通观察返回**（`err == nil`），因为要让模型自己改参数重试。
结果就是 `executeTool` 不报错 → `res.succeeded == true` → 按 `res.succeeded` 判定会把
**每一次失败的尝试都算成一次写入**。典型场景：`edit_file` 的 `old` 文本没匹配上，模型重试
三次，客户端最终会看到「编辑 1 个文件 +36 −12」，而实际只改了一次。

已核对的失败返回（全部 `nil error`）：

| 工具 | 失败观察（`res.observation` 前缀） | 有无写入 |
|---|---|---|
| `apply_patch` | `Patch did not apply; no files were changed.` | 无（`:99`/`:109` 两处） |
| `edit_file` | `Could not find the 'old' text…` / `The 'old' text appears N times…` / `Refused: 'new' is empty…` | 无 |
| `edit_file` | `Could not open…` / `is a directory…` / `File too large…` / `not a UTF-8 text file.` / `Access to external path was denied…` | 无 |
| `edit_file` | `The 'old' text is empty…`（`:84`）/ `The 'old' and 'new' text are identical; nothing to change.`（`:87`）—— 两条空操作，极易被漏进单测 | 无 |
| `insert_file` | 同上那一组（`Access to external path was denied…` / `Could not open…` / `is a directory…` / `File too large…` / `not a UTF-8 text file.`） | 无 |
| `insert_file` | 行号越界：`insertWholeLines` 的 error 原样回显（`… has N line(s). Line 0 inserts at the top…`，`:135`） | 无 |
| `insert_file` | `Refusing the insert: … would grow to N bytes (max …).`（`:140`） | 无 |
| `create_file` | `<path> already exists. Use edit_file to modify an existing file.` / `Content too large…` / `is a directory…` | 无 |

两种做法，**推荐 A**：

- **A（结构化，推荐）**：给 `tools.ToolResult` 加 `Wrote []tools.FileWrite`
  （`FileWrite` 定义在 `internal/tools/diffstat.go`，`agent.DiffFile` 是它的别名），
  在这四个工具各自的**成功返回点**填（`edit_file.go:177`、`create_file.go:108`、
  `insert_file.go:152`、`apply_patch.go:112`），`commitToolResult` 只读这个字段。
  好处：以后改文案、加新工具都不会静默失效；工具自己知道 `m.tolerant`、`lineCount` 这类只有它掌握的信息。
  行数不要另写一套：`create_file.go:104-107` 的 `lineCount` 就是本文 §4.3 要的口径
  （`strings.Count(content,"\n")`，无结尾换行且非空时 +1），直接复用或抽成公共函数。

  已核对：这四个插入点各自是**唯一**成功返回点（`edit_file.go:177`、`create_file.go:108`、
  `insert_file.go:152`、`apply_patch.go:112`），其前面所有 `return tools.ToolResult{...}, nil`
  都是失败观察。`apply_patch` 的 `execApplier` 与 gogit 两个 backend 共用 `:112` 这一个成功出口，
  所以成功判定不需分 backend —— 统计仍从入参 patch 解析，与 backend 无关。

  方案 A 的两条附加约束：

  1. **只从内置工具读**：`commitToolResult` 里按工具名白名单（这四个）读取该字段，不要让它变成
     「任意工具都能写的通道」。将来若有客户端工具/MCP 结果整体反序列化成 `tools.ToolResult`
     （目前没找到这种路径），该字段就会成为伪造入口。
  2. **字段语义是「已落盘的写入」**，不是「打算写」：工具在 `os.WriteFile`/applier 返回成功之后才填。
- **B（省事但脆）**：在 `commitToolResult` 里对这四个工具名做一次 `strings.HasPrefix` 白名单反向判断。
  能跑，但任何文案改动都会让统计静默归零，且和 §4.3「不解析输出文案」的原则自相矛盾。

另外注意 `edit_file` 可能走**容错匹配**（`locateEdit` 的 tier 2/3：去行号前缀、折叠全角标点），
此时真正被替换的 `m.matched` 与入参 `old` 不完全相同。按入参 `old` 计行数是可接受的近似
（容错层不改变行结构），但如果走方案 A，直接用 `m.matched` 计数最准。

### 4.3 各工具取数来源（用入参或有结构的返回值，**不解析文案**）

| 工具 | 入参 | 计法 |
|---|---|---|
| `apply_patch` | `{patch}`（unified diff） | 用已在依赖里的 `github.com/bluekeyes/go-gitdiff` 解析 hunk，逐文件统计 `+`/`-`；仅当实际应用成功才计入（`Patch did not apply` 不计） |
| `edit_file` | `{path, old, new, allow_delete}` | `added = 行数(new)`，`removed = 行数(old)`。`allow_delete` 只表示「空 `new` 是在删文本，不是被截断的调用」，**不删文件**，状态恒为 `modified`；`old` 不唯一或找不到时工具已拒绝（4.2.1），不计 |
| `insert_file` | `{path, line, content}` | `added = 行数(content)` |
| `create_file` | `{path, content}` | `added = 行数(content)`，`status = added` |

行数口径：按 `\n` 切分后忽略末尾空行，与 apply_patch 的 hunk 计数保持一致（否则「patch 新建文件」
和「create_file 新建文件」两种写法会差 1 行）。

### 4.4 嵌套 / 子代理（容易漏，必须做）

`internal/agent/nested_tool.go:17` 的 `ExecuteNestedTool` 是**独立路径**，不经过 `commitToolResult`。
workflow 节点、子代理里的写入必须冒泡到父 turn，否则父轮显示「编辑 0 个文件」而实际改了代码。

`NestedUsages` 通道确实存在（`internal/tools/tool.go:338`），但它**当前的唯一写入方是 Flux 复合工具
（`internal/runtime/flux_tool.go:761` 的 `t.usage.add(result.Usage)`），不是 `ExecuteNestedTool`**：
嵌套结果先被 flux 节点消费，再由 flux 工具汇总成自己的 `ToolResult`（`:269`），最后才在
`tool_batch.go:313` 合并到父轮。所以只加两个字段不会生效，必须沿同一条链补五处：

| 位置 | 动作 |
|---|---|
| `internal/tools/tool.go:338`（`NestedUsages` 下一行） | `ToolResult` 加 `NestedChanges []DiffFile` |
| `internal/agent/nested_tool.go`（`ExecuteNestedTool` 返回前） | 该次嵌套调用自身的写入填入返回值的 `NestedChanges` |
| `internal/runtime/flux_tool.go:761`（`t.usage.add(result.Usage)` 处） | 加 `nestedChangeCollector`（对照 `:835` 的 `nestedUsageCollector`），`add(result.NestedChanges)` |
| `internal/runtime/flux_tool.go:269`（`NestedUsages: nestedUsage.snapshot()` 处） | 同处输出 `NestedChanges: changes.snapshot()` |
| `internal/agent/tool_batch.go:313`（`res.nestedUsage = toolResult.NestedUsages` 处） | 同处合并 `res.nestedChanges`，并入 `turn.DiffStat` |

**第二个入口：`flux_tool.go:1089`（`FluxWorkflowTool.executeRetry`）不用改语义，但签名要跟着变。**
它同样 new 了一个 `nestedUsageCollector`，可是这条路径本来就**不返回** `NestedUsages`
（只回一段状态 payload），所以嵌套记账在这里从来是丢的。实现按「与 NestedUsages 保持一致」
处理：传入 `nil` collector，不额外造一套只在这一条路上生效的统计。要真正修，得先让 retry 也
返回结构化 telemetry，那是独立的一件事。

**第三个入口可以不管**：`internal/runtime/headless.go:135` 也 new 了一个 collector 并直接丢弃，
但 headless 走的是 `rt.Submit(workflow)`，**不产生 conversation turn 终态事件**，没有 `turn_finished`
可挂 diffstat，不在 v1 范围。别顺手改它，改不出可见效果。

### 4.5 终态发射点（3+2 处）

| 位置 | 场景 |
|---|---|
| `internal/agent/loop.go:1180` | 正常结束（已带全量计数） |
| `internal/agent/loop.go:1260` | 步数上限后的兜底答案（已带全量计数） |
| `internal/conversation/executor.go:1288` | `emitLifecycle`（paused/failed/cancelled，事件字面量 1304–1309 行）：**failed / cancelled 带 `result.DiffStat`，paused 必须留空**（§2 硬性要求 2；paused 之后恢复还会再发 `turn_finished`，提前发一份半截统计会让客户端看到两次不一致的数字） |
| `internal/agent/loop.go:1243`、`:1250` | 两条只带 `Text` 的步数上限分支（现在连计数都没有，顺手补齐） |

失败/取消路径可用：loop 的错误分支（`989`/`994`/`1094`/`1115`/`1226`）返回的是**部分填充的 `turn`**，
不是空结构，所以 `emitLifecycle` 拿到的 `result.DiffStat` 已含本轮已发生的写入。只有工具执行前的
早期错误（`TurnResult{}`）为空，那时确实没有写入。

### 4.6 序列化

`internal/server/wire.go`：`wireEvent` 加 `DiffStat *agent.TurnDiffStat \`json:"diffstat,omitempty"\``
（结构体字段区，`executed_tool_calls` 同级，声明在 **90 行**），并在 `toWire`（133 行）的 Event → wire
映射里拷贝（**184 行**，与 `ExecutedToolCalls: e.ExecutedToolCalls,` 相邻）。持久化层存的是
`session.EventRecord.Payload`（wire frame 的原始 JSON），所以字段加在 `wireEvent` 上即可自动回放，
无需动 store。

## 5. 验收标准

单测（建议放 `internal/agent/*_test.go` + `internal/server/wire_test.go`）：

- [ ] `apply_patch`：单文件 / 多文件 / 新建 / 删除 / 应用失败（=0）
- [ ] `edit_file`、`insert_file`、`create_file` 各一条
- [ ] 同一文件两次 `edit_file` → 累加
- [ ] **失败不计（重点，见 4.2.1）**：`apply_patch` 应用失败、`edit_file` 的 `old` 未匹配 / 匹配多次 /
  `Refused: 'new' is empty` / `'old'` 为空 / `'old'` 与 `'new'` 相同、`insert_file` 体积拒绝、
  `create_file` 目标已存在 —— 这些 `err == nil`，必须逐条断言不计数
- [ ] 被拒绝（用户审批否决）的调用不计（`res.executed == false`）
- [ ] `ExecuteNestedTool` 的写入汇总到父轮
- [ ] wire JSON golden：有变更时的完整结构 + 无变更时字段完全不出现

端到端：

- [ ] 跑一轮真实任务（含一次 apply_patch 新建 + 一次多次 edit），用
      `git diff --numstat` 对照 `added_lines/removed_lines`；差异只应来自「同文件累加」与
      「run_command 写入」，并在 PR 描述里写清实测数字
- [ ] 老事件回放（无 diffstat）正常返回，不影响现有 wire 契约测试

## 6. 交付与上线顺序

1. 后端 PR：计数 + 契约 + 测试，附一份示例 JSON 到 `docs/contracts/turn-diffstat-example.json`。
   **§4.2.1（计数正确性）与 §4.4（嵌套冒泡）必须在同一个 PR 内发**：折叠头文案最终写
   「本轮编辑 N 个文件」、不带「主流程」限定，这个文案只有在嵌套写入确实冒泡后才是真的。
   分两次发会出现「文案已经敢说全量、但子代理写入还统计不到」的窗口。
2. 客户端 PR（我们这边，字段上线后接）：`WireFrame` → `AgentEvent` → `ConversationTurn` →
   文档 `Turn.changes` → 折叠头文案 `用时 24分40秒 · 本轮编辑 3 个文件`；已有注释位，改动很小。
3. **服务端先发**（客户端忽略未知字段，无破坏），客户端后接。

## 7. 决策记录与待确认项

### 已定（不再讨论）

1. **计数方案取 A**：`tools.ToolResult` 加结构化写入字段，四个内置写工具在成功返回点填，
   `commitToolResult` 读它。放弃 B —— 失败观察文案 20+ 条、分布在两个 `apply_patch` backend，
   `HasPrefix` 黑名单必然漏项，且与 §4.3「不解析文案」冲突。
   附加约束两条，一并生效：**① 只从这四个工具名白名单读该字段**（否则它是任意工具可写的伪造通道）；
   **② 字段语义是「已落盘」**，必须在 `os.WriteFile` / applier 返回成功之后才填。
2. **失败重试一律不计**，按 §4.2.1 与 §5 逐条断言，`res.succeeded` 不可作为判定依据。
3. **§4.2.1 与 §4.4 同一 PR**（§6），否则折叠头文案「本轮编辑 N 个文件」在嵌套路径上先于事实成立。
4. **`turn_paused` 不带 diffstat**（§2 硬性要求 2、§4.5）。

### 仍需确认

1. `files` 截断 200 条是否够用（超长 turn 可能触及）。
2. v2 是否愿意做净变化（turn 开始快照 + 结束时对比），用来消除「同文件累加」的偏差。
3. `run_command` 的写入要不要覆盖（需要 git/fs 快照，成本明显更高，v1 建议不做）。
