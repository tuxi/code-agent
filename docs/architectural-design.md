Claude Code 架构调研 2026-7-31

### 1. System Prompt 分层

当前会话的 system prompt 有明确的层次：

Layer 0: Harness — 工具定义（28 个工具，JSON Schema + 使用规则）
Layer 1: Environment — cwd, git status, branch, 最近 5 次 commit, OS, shell
Layer 2: Session guidance — model, session ID, permissions, hooks, current date
Layer 3: Project context — CLAUDE.md/AGENTS.md + Memory 召回（按需）
Layer 4: System reminders — 动态注入，不被持久化

关键发现：工具描述不在 system prompt 文本里，在 tools JSON 字段里。 system prompt 只写"怎么用工具"，不写"有哪些工具"。这和 code-agent 的 API tools 字段是一致的——Pi 反而是那个把工具列表写在 system prompt 文本里的异类。

### 2. LSP：工具封装，不是 Agent 能力

核心设计决策：Agent 不实现 LSP 协议。LSP 是宿主进程的能力，Agent 只通过标准工具调用消费它——和 `read_file`、`grep` 完全一样。

架构分层：

```
Claude Code 宿主进程
│
├─ LSP Client 模块（独立，Go/Node 实现，不在 agent 里）
│   ├─ Server 发现：检测项目语言 → 匹配 LSP server
│   │     go.mod     → gopls
│   │     tsconfig   → tsserver / typescript-language-server
│   │     Cargo.toml → rust-analyzer
│   │     pyproject  → pyright
│   │
│   ├─ Server 管理：懒启动、自动重启、连接健康检查
│   │     首次调用 LSP 工具时启动子进程
│   │     server 崩溃 → 下次调用自动重启
│   │     完全透明对 agent
│   │
│   └─ 协议层：JSON-RPC over stdin/stdout
│        初始化握手 → 监听诊断 → 按需查询
│
└─ Agent（LLM）
    │
    │  可用 LSP 工具（来自 system prompt 最顶部的工具列表）：
    │
    ├── goToDefinition(file, line, col) → URI + range
    │    我： "goToDefinition('loop.go', 26, 10)" → "loop.go:26:10 → plan.go:14:5"
    │    用户： 看不见这个调用，只看到我告诉他 "Runner 在 plan.go 第 14 行定义的"
    │
    ├── findReferences(file, line, col) → []Location
    │    我： "findReferences('loop.go', 144, 20)" → 找到 5 处引用
    │    用户： 看到我说 "plannedMutation 在 3 个文件和 2 个测试中被引用"
    │
    ├── hover(file, line, col) → 类型信息 + 文档
    │    我： "hover('builder.go', 42, 18)" → "func (b *Builder) Build() (*Session, error)"
    │    用户： 看到我精确引用了函数签名和文档注释
    │
    ├── documentSymbol(file) → 文件大纲
    │    我： "documentSymbol('main.go')" → 12 个函数、3 个类型、1 个常量
    │    用户： 看到我快速列出了文件结构
    │
    └── workspaceSymbol(query) → 全局搜索
         我： "workspaceSymbol('NewFauxHarness')" → 找到定义位置
         用户： 看到我直接定位了符号，不需要先 grep
```

Agent 的视角：

```
我是 agent。当我需要理解代码结构时，我有两个选择：

选择 A（没有 LSP）：
  grep "func NewFaux" → 找到定义行
  read_file 附近区域 → 读函数签名
  grep "NewFauxHarness(" → 找到调用点

选择 B（有 LSP）：
  workspaceSymbol("NewFauxHarness") → 一行调用，精确定位
  findReferences(...) → 所有调用点 + 定义点，结构化返回
  hover(...) → 完整函数签名 + 文档注释

LSP 让 '理解代码结构' 这个任务从 3-5 步变成 1-2 步。token 消耗更低，因为 LSP 返回的是结构化位置信息，而不是整段代码。
```

对 code-agent 的启示：

code-agent 已经有 `project_graph` 包和 `LanguageAdapter` 接口，走到了半路：

```
当前：
  ✅ LanguageAdapter 接口（FindSymbol, FindReferences）
  ✅ gopls.go（Go，CLI 子进程模式）
  ✅ tool.go（find_symbol, find_references 工具）

缺失：
  ❌ Hover（类型/文档信息）
  ❌ Diagnostics（编译器错误/警告）
  ❌ documentSymbol（文件结构大纲）
  ❌ tsserver / rust-analyzer / pyright 适配器
  ❌ 自动发现（检测项目语言 → 匹配适配器）
```

实现路径不需要引入 LSP 协议——继续走 CLI 子进程模式：

```
gopls definition file.go:10:5    → 结构化 JSON → 解析为 Location
gopls references file.go:10:5    → 同上
gopls hover file.go:10:5         → 类型信息文本
gopls diagnostics file.go        → 编译错误列表
```

所有 LSP server（gopls, tsserver, rust-analyzer, pyright）都支持 CLI 模式。这不比 `runGitCmd` 更复杂。

### 3. Plan Mode 触发机制

我不是"自动判断是否进入 plan mode"——我有一套明确的启发式规则写在 system prompt 里：

Plan mode — for complex tasks, RESEARCH first, then IMPLEMENT:
- If the task involves implementing a new feature, spans multiple files,
  involves architecture decisions, or has unclear requirements, call
  enter_plan_mode FIRST

For simple, well-scoped changes, skip it and act directly.

但真正让我"不滥用 plan mode"的不是这段文字，而是另一条规则：

Keep answers short and concise.
When you have enough information to act, act.
Do not re-derive facts already established.

这两条合在一起的效果：Plan mode 有明确触发条件，但"直接行动"是默认值。这很接近刚才 code-agent 改成的 AND 逻辑。

### 4. 完成验证：我的 Stopping 策略

system prompt 里有详细的 phase-aware stopping 规则。但我还有一种不写在 prompt 里、宿主进程强制执行的验证：

我的 side-effecting 工具（edit/write/bash）在执行前：
├─ 权限检查（settings.json 的 allow/deny 规则）
├─ approval check（auto mode 可能跳过）
└─ Hook 检查（pre/post tool hooks）

我的"做完了"之后：
└─ 没有 Reflector 式的自动验证 ← 这是我靠自觉做的！

Claude Code 其实没有 code-agent 的 Reflector 自动验证。 我之所以每次都跑一下 build/test，是因为 system prompt 告诉我要这样做——但如果一个模型的 system prompt compliance 不好，它可能跳过验证。code-agent 的 Reflector 把这一步变成了 loop 里的硬编码检查，比 Claude Code 更可靠。


### 5. Memory 系统

```
~/.claude/projects/<project>/memory/
  ├── MEMORY.md  ← 索引文件
  └── <slug>.md  ← 每个记忆一个文件，带 YAML frontmatter
```

注入机制：
这和 Pi 的 ~/.pi/agent/AGENTS.md（全局规则文件全量加载）不同。Memory 是按需的——不常用的记忆不占上下文。code-agent 没有这个。

### 6. 上下文压缩

我每次被调用时：
├─ 如果对话超过阈值 → 自动压缩
├─ 压缩保留：system prompt + 最近的 K 条消息 + 摘要
└─ 摘要是累积的（新摘要合并到旧摘要）

这和 code-agent 的 LLMCompactor 一样，都是 LLM 驱动的压缩。但我还有一个context editing——清除旧的 tool 结果和 thinking，这是在压缩之前的一步轻量清理。

---
对 code-agent 的启示

┌───────────────────────────────┬───────────────────────────────────────────┬────────┐
│          claude code已做到的           │             code-agent 可借鉴             │ 优先级 │
├───────────────────────────────┼───────────────────────────────────────────┼────────┤
│ LSP 作为工具（不在 agent 内） │ 扩展现有 LanguageAdapter，不改 agent 核心 │ P1     │
├───────────────────────────────┼───────────────────────────────────────────┼────────┤
│ Memory 按需召回               │ 替代全量加载的全局 AGENTS.md              │ P2     │
├───────────────────────────────┼───────────────────────────────────────────┼────────┤
│ Context editing 轻量清理      │ 在 LLMCompactor 之前加一步剪枝            │ P3     │
├───────────────────────────────┼───────────────────────────────────────────┼────────┤
│ Plan mode 不滥用              │ 已做（AND 逻辑 + "直接行动是默认"）       │ ✅     │
├───────────────────────────────┼───────────────────────────────────────────┼────────┤
│ 完成验证                      │ Reflector 已比 Claude Code 更强           │ ✅     │
├───────────────────────────────┼───────────────────────────────────────────┼────────┤
│ Agent 不实现 LSP              │ LanguageAdapter + CLI 子进程模式正确      │ ✅     │
└───────────────────────────────┴───────────────────────────────────────────┴────────┘