# code-agent RAG 功能设计

> 基于 Claude Code / Cursor / GitHub Copilot 的 RAG 实践调研
> 日期: 2026-06-30

## 目录

1. [行业调研](#1-行业调研)
2. [核心设计决策](#2-核心设计决策)
3. [架构设计](#3-架构设计)
4. [Tier 1: code_search — 代码语义搜索](#4-tier-1-code_search--代码语义搜索)
5. [Tier 2: kb_search — 外部知识库](#5-tier-2-kb_search--外部知识库)
6. [Tier 3: 云端 embedding（可选）](#6-tier-3-云端-embedding可选)
7. [实现计划](#7-实现计划)
8. [端侧与服务端的使用场景](#8-端侧与服务端的使用场景)
   - [8.1 iOS 端 RAG 的价值重定义](#81-ios-端-rag-的价值重定义)

---

## 1. 行业调研

### 三大主流 Agent 的 RAG 实践

| | Claude Code | Cursor | GitHub Copilot |
|---|---|---|---|
| **语义代码搜索** | ❌ 不做，依赖 grep + read_file | ✅ 最成熟 | ✅ 自动索引 |
| **外部知识库** | ✅ MCP doc server + CLAUDE.md + Skills | ❌ | ❌ |
| **嵌入模型** | 不使用嵌入 | 自研，Agent trace 训练 | 自研 Matryoshka（+37.6% 质量提升） |
| **嵌入存储** | N/A | 云端 Turbopuffer | 本地 + 云端混合 |
| **代码分块** | N/A | AST-aware | AST-aware (Tree-sitter) |
| **增量索引** | N/A | Merkle tree（~3 分钟同步） | 秒级重索引 |
| **隐私策略** | 本地优先 | 向量云端，源码仅本地 | 混合 |
| **语义 + grep** | “grep 胜嵌入” | ✅ Hybrid | ✅ 两者兼有 |

### 三个行业共识

**1. Grep 胜过嵌入——用于精确代码搜索**

Cursor 明确声明：_"语义搜索和 grep 的结合才能达到最佳效果。"_ Copilot 的 Agent 模式在知道确切名称时优先用 grep。Claude Code 完全不做语义索引，完全依赖 grep + read_file + project_graph 的探索式检索。

语义搜索的价值在**探索性查询**：你不知道确切的函数名、文件名，只知道"大概在做什么"。此时 `grep "memory"` 返回 200 行，但 `code_search("内存管理核心逻辑")` 返回 3 个最相关的函数。

**2. 嵌入可以上云，源码必须本地**

Cursor 的架构：向量存在云端 Turbopuffer，但源码仅以混淆的文件路径 + 行号形式存储。查询时服务器返回文件位置，客户端本地读取代码，发送给 LLM 后即丢弃。

Copilot 类似：索引自动在云端构建，但代码内容受企业内容排除策略控制。

对 code-agent 的启示：我们有条件做得**更极致**——全本地。130MB 的 gte-small 嵌入模型在 M1 Pro 上跑单个 chunk < 50ms，全量索引一个 10 万行项目 < 30 秒，不需要云端。

**3. Agentic RAG > Pipeline RAG**

不要预先计算好上下文注入 prompt。让 Agent 自己决定**何时**检索、**检索什么**。把 `code_search` 和 `kb_search` 注册为普通工具，模型在需要时主动调用：

| 时代 | 范式 | 做法 |
|------|------|------|
| 2023 | Pipeline RAG | 用户提问 → 固定检索 → 拼接 prompt → LLM 回答 |
| 2024 | Chat RAG | 多轮对话 + 人工选择检索范围 |
| 2025-2026 | **Agentic RAG** | 模型自己决定 IF / WHAT / WHERE / HOW 检索 |

---

### Claude Code 的关键发现：上下文工程 > Prompt 工程

LangChain 2025 年 9 月的对照实验发现：

| 配置 | 效果 |
|------|------|
| Claude 裸跑 | 基线 |
| Claude + MCP 文档服务器 | 轻微提升 |
| Claude + CLAUDE.md（精选指南）| **显著提升** |
| Claude + MCP + CLAUDE.md | 最佳 |

**高质量的精选信息 + 需要细节时再查的工具** = 最优模式。原始文档转储（`llms.txt`）反而**降低**性能——填充了宝贵的 context window。

对 code-agent 的启示：
- 已有的 Skills 系统 = CLAUDE.md 等价物（精选知识，始终加载）
- RAG 应该作为**工具提供**，需要时调用，不要预加载进 context
- 不要让 RAG 结果挤占 agent loop 本已紧张的上下文

---

### Cursor 的核心架构

```
源码 (客户端)
    │ 语法感知分块 (AST/Tree-sitter)
    ▼
嵌入生成 (Cursor 自研模型, GPU 云端)
    │
    ▼
向量存储 (Turbopuffer, 多租户云端)
    │ 仅存嵌入 + 混淆路径 + 行号，源码不落盘
    ▼
@Codebase 查询
    │ 用户问题 → 嵌入 → 相似度搜索 → chunk ID
    ▼
客户端取回源码 → 发送给 LLM → LLM 回复后即丢弃
```

关键创新：
- **Merkle tree 增量索引**：每 ~3 分钟比对 client/server 的 Merkle tree，仅重索引变更文件
- **自研嵌入模型**：用生产 Agent trace 训练——Agent 搜索了什么、最终用了什么、LLM 事后评判该检索什么
- **隐私模式**（50% 用户开启）：源码不保留超过单次请求

---

### GitHub Copilot 的核心架构

```
源码 (workspace)
    │
    ▼
分块 (AST-aware / Tree-sitter, 语义单元: 函数/类/方法)
    │
    ▼
嵌入 (Copilot 自研 Matryoshka 模型, +37.6% 质量提升)
    │
    ▼
向量存储 (本地索引, HNSW)
    │
    ▼
检索 (top-k 相关代码段)
    │
    ▼
Prompt 组装 → LLM 响应
```

关键创新：
- **Matryoshka 嵌入**：灵活的嵌入维度（不需要重新训练就能降维）
- **Hard negative mining**：训练模型区分"看起来差不多"和"真的相关"
- **8x 更小的索引**：同样的质量，索引体积减少 87.5%
- **秒级索引**：大仓库最多 60 秒完成首次索引

---

## 2. 核心设计决策

### 决策 1：全本地 vs 混合云端

```
选择：全本地

理由:
  ✅ code-agent 的 iOS/macOS 端侧定位与隐私优先理念一致
  ✅ gte-small (130MB) 在 M1 Pro 上 < 50ms/查询，性能足够
  ✅ 零网络依赖 = 离线可用 + 零隐私风险
  ✅ SQLite + vec0 扩展可纯 Go 实现，无外部服务依赖
  ⚠️ 云端 embedding API 作为可选项 (Tier 3, 后期)
```

### 决策 2：Agentic RAG（工具驱动）vs Pipeline RAG（预计算）

```
选择：Agentic RAG

理由:
  ✅ code-agent 已有成熟的 Agent loop + 工具注册机制
  ✅ code_search / kb_search 注册为普通工具，模型主动调用
  ✅ 不预加载检索结果——节约稀缺的 context window
  ✅ 模型可以多轮检索、逐步缩小范围
```

### 决策 3：AST 分块 vs 固定大小分块

```
选择：AST-aware 分块 (Tree-sitter)

理由:
  ✅ Copilot 和 Cursor 都验证了 AST 分块优于固定大小
  ✅ 一个完整函数作为 chunk = 语义完整的最小单元
  ✅ Go 生态已有成熟的 Tree-sitter 绑定
  ✅ 配合 project_graph 已有的 AST 解析能力
```

### 决策 4：嵌入 vs 不嵌入（Claude Code 路线）

```
选择：嵌入，但不替代 grep

理由:
  ✅ grep 仍然是精确搜索的首选——code_search 是补充，不是替代
  ✅ Cursor 和 Copilot 都验证了 hybrid 模式的价值
  ✅ 当用户/模型不确定精确名称时，语义搜索是唯一选择
  ❌ 纯 grep 路线（Claude Code）在"我不知道搜什么"场景下无解
```

---

### 与 project_graph 的关系：互补，不是替代

code_search (RAG) 和 project_graph (LSP) 解决不同层次的问题：

| | project_graph | code_search |
|---|---|---|
| **技术** | LSP 语言工具链 (gopls/sourcekitten/rust-analyzer) | 嵌入向量 + 语义相似度 |
| **查询方式** | 精确符号名 `find_symbol("dealloc")` | 自然语言 `"内存管理核心逻辑"` |
| **前置条件** | 必须知道确切的符号名 | 不需要知道任何符号名 |
| **外部依赖** | 需安装对应语言工具链 | 无（纯嵌入式） |
| **输出** | 结构化符号数据 (签名/位置/引用关系) | 相关代码片段 + 相似度分数 |
| **最佳场景** | 已知符号 → 追踪引用链 → 重命名分析 | 不知道符号名 → 探索式发现 → 定位入口 |

两者结合 = Cursor/Copilot 的 hybrid 模式：

```
用户: "ARC 下对象什么时候释放？"

  step 1: code_search("ARC 内存管理 对象释放")
           → 发现 RetainCountManager.decrement(), MyObject.dealloc
           （不知道函数名，语义搜索帮他发现入口）

  step 2: project_graph.find_references("RetainCountManager")
           → LSP 精确追踪所有调用 RetainCountManager 的位置
           （知道符号名了，编译器级精确分析调用链）

  step 3: project_graph.find_symbol("dealloc")
           → LSP 找到 dealloc 的完整签名和实现细节
```

code-agent 现有的 grep + project_graph + read_file 已经覆盖了**精确搜索**和**符号追踪**。code_search 补上的是缺失的一环：**当你不知道搜什么的时候，用自然语言发现入口**。

---

## 3. 架构设计

### 整体架构

```
                         code-agent Agent Loop
                                 │
            ┌────────────────────┼────────────────────┐
            ▼                    ▼                    ▼
       code_search          kb_search          grep / read_file
    "这段逻辑在哪里?"    "ObjC block 语法?"  "grep -r 'delegate'"
            │                    │                    │
            ▼                    ▼                    │
   ┌─────────────────────────────────────────────────┘
   │
   ▼
┌─────────────────────────────────────────────────────────┐
│                  RAG Engine (Go 进程内)                   │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │  Embedding   │  │   Vector     │  │   Chunker     │  │
│  │  Model       │  │   Store      │  │               │  │
│  │              │  │              │  │  Tree-sitter   │  │
│  │  gte-small   │  │  SQLite      │  │  AST 解析      │  │
│  │  ONNX 运行时  │  │  + vec0 扩展  │  │  函数/类/方法  │  │
│  │  ~130 MB     │  │  纯 Go       │  │  分块          │  │
│  └──────────────┘  └──────────────┘  └───────────────┘  │
│                                                         │
│  索引层                                                 │
│  ┌──────────────────────────────────────────────────┐   │
│  │  代码索引 (namespace: code_)   知识库索引 (kb_)    │   │
│  │  - file_path                  - source           │   │
│  │  - function_name              - title            │   │
│  │  - line_range                 - tags             │   │
│  │  - chunk_content              - chunk_content    │   │
│  │  - embedding (384d)           - embedding (384d) │   │
│  │  - updated_at                 - updated_at       │   │
│  └──────────────────────────────────────────────────┘   │
│                                                         │
│  增量更新: 文件 mtime 比对，仅重索引变更文件               │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │  端侧 (Mac/iOS): 全部本地，零网络，零隐私风险       │   │
│  │  云端 fallback (可选 Tier 3): embedding API       │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 组件清单

| 组件 | 技术选型 | 大小/性能 | 依赖 |
|------|---------|----------|------|
| 嵌入模型 | gte-small (ONNX) | 130 MB, < 50ms/chunk | ONNX runtime (CGo) |
| 向量存储 | SQLite + vec0 扩展 | 纯 Go | mattn/go-sqlite3 |
| 代码分块 | Tree-sitter | 已有 project_graph 可复用 | smacker/go-tree-sitter |
| 增量索引 | 文件 mtime + hash | O(changed) 而非 O(all) | 标准库 |

---

## 4. Tier 1: code_search — 代码语义搜索

### 解决什么问题

"那段处理内存管理的逻辑在哪里？"——grep `memory` 返回 200 行，code_search 返回 3 个最相关的函数。

### 索引流程

```
源码文件 (.go, .swift, .m, .py, ...)
    │
    ▼ Tree-sitter 解析 AST
    │
    ▼ 提取语义单元: function_declaration, method_declaration, class_declaration
    │
    ▼ 每个语义单元 → 一个 chunk { path, name, start_line, end_line, content }
    │
    ▼ gte-small embedding → 384 维向量
    │
    ▼ 存入 SQLite: (id, namespace, file_path, chunk_content, embedding BLOB, mtime)
    │
    ▼ 增量更新: 对比每个文件的 mtime，仅重索引变更文件
```

### 查询流程

```
用户/模型调用 code_search(query="内存管理核心逻辑", top_k=5)
    │
    ▼ query → gte-small → query_embedding (384d)
    │
    ▼ SQLite: SELECT *, cosine_similarity(embedding, query_embedding) AS score
    │            FROM chunks WHERE namespace='code'
    │            ORDER BY score DESC LIMIT top_k
    │
    ▼ 结果过滤: score < 阈值 → 返回 "no relevant results, try grep"
    │
    ▼ 返回 [{file_path, function_name, line_range, content, score}, ...]
```

### 工具定义

```go
Tool{
    Name: "code_search",
    Description: `Semantically search the codebase for relevant code chunks.
Use this when you know WHAT a piece of code does but not WHERE it is or what it's called.
For exact symbol/string searches, use grep instead — it's more precise.
Returns the top matching functions/methods with file paths and line ranges.`,
    Parameters: {
        "query":  "Natural language query describing what the code does, e.g. 'memory management for ARC objects'",
        "top_k":  "Number of results to return (default 5, max 10)",
    },
}
```

### Hybrid 降级策略

```
if max(results.score) < 0.6:
    suggest: "No strong semantic matches. Try grep with relevant keywords."
if len(results) == 0:
    suggest: "No results found. Consider broader query or grep."

# 所有 code_search 结果都附带对应的 grep 建议关键词
```

---

## 5. Tier 2: kb_search — 外部知识库

### 解决什么问题

用户的学习资料、API 文档、团队规范不在代码库里。"ObjC 的 block 语法是什么？"——grep 代码库搜不到，但学习笔记里有。

### 知识摄入

```
Markdown/文本文件 (.md, .txt)
    │
    ▼ 按 ## 标题分块 (保持语义完整性)
    │
    ▼ 每个 section → 一个 chunk { source, title, content }
    │
    ▼ gte-small embedding → 384 维向量
    │
    ▼ 存入 SQLite: namespace='kb' (独立于 code_ 命名空间)

元数据:
  {
    source:   "objc-learning/blocks.md",  // 来源文件
    title:    "Block 语法",               // 章节标题
    tags:     ["objc", "blocks", "syntax"], // 标签 (可选)
    added_at: "2026-06-30T12:00:00Z"
  }
```

### 工具定义

```go
Tool{
    Name: "kb_search",
    Description: `Search the external knowledge base (learning materials, API docs, team conventions).
Use this when the user asks a conceptual question or needs documentation that isn't in the codebase.
The knowledge base contains curated reference materials, not code.`,
    Parameters: {
        "query":  "Search query in natural language",
        "source": "Optional: limit to a specific knowledge base (e.g. 'objc', 'swift')",
        "top_k":  "Number of results (default 3, max 5)",
    },
}
```

### 与 Skills 的互补关系

Skills 和 RAG 解决不同的问题，不是替代关系：

```
Skill (SKILL.md):
  → 始终加载到 system prompt 的精选指令
  → "写 ObjC 代码时遵循这些规范"
  → 静态、精选、始终可见

RAG (kb_search):
  → 需要时才调用的动态检索
  → "Block 循环引用有哪三种解决方案？"
  → 动态、按需、结果注入当前轮次
```

理想状态：Skill 告诉模型**怎么做**，kb_search 让模型**查资料**。

```
skills/objc-learning/
├── SKILL.md               ← 静态指令（始终加载）
│   "你是 ObjC 专家。写代码时遵循 Apple 的内存管理规范..."
│   "遇到不确定的 API 用法时，优先调用 kb_search 查知识库。"
│
└── knowledge/              ← 动态知识（按需检索）
    ├── blocks.md
    ├── runtime-and-arc.md
    ├── protocols.md
    └── foundation.md
```

### 知识摄入方式

```
方式 1: 文件导入
  codeagent kb import ./my-notes/*.md

方式 2: Skills 绑定
  skills/objc-learning/knowledge/  ← 自动索引目录下所有 .md

方式 3: URL 导入 (后期)
  codeagent kb import-url https://developer.apple.com/documentation/...
```

---

## 6. Tier 3: 云端 embedding（可选）

端侧 embedding 完全够用。但如果需要更强的嵌入或节省 130MB 内存：

```go
// Embedder 接口 — 本地/云端统一抽象
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
    Dims() int  // 384 for gte-small
}

// 本地 ONNX 嵌入器 (默认)
type LocalEmbedder struct {
    session *onnx.Session  // gte-small, ~130MB
}

// 云端嵌入器 (可选)
type CloudEmbedder struct {
    client  *http.Client
    baseURL string
    apiKey  string
}

func NewEmbedder(cfg RAGConfig) Embedder {
    if cfg.CloudEmbedURL != "" {
        return &CloudEmbedder{...}  // 用户显式配置才用云端
    }
    return &LocalEmbedder{...}      // 默认本地
}
```

---

## 7. 实现计划

### Phase 1: 核心 — code_search（预估 3-5 天）

```
□ internal/rag/embedder.go       — Embedder 接口 + gte-small ONNX 实现
□ internal/rag/chunker.go        — Tree-sitter AST 分块器
□ internal/rag/vector_store.go   — SQLite + vec0 向量存储
□ internal/rag/indexer.go        — 全量 + 增量索引器
□ internal/rag/searcher.go       — 语义搜索 + Hybrid 降级
□ internal/rag/tool.go           — code_search 工具注册
□ internal/rag/config.go         — RAG 配置 (索引路径、嵌入模型等)
```

**依赖**：
- `github.com/nickgasior/gomobile-onnx` (ONNX runtime Go 绑定)
- `github.com/smacker/go-tree-sitter` (已有项目依赖)
- `github.com/mattn/go-sqlite3` (已有项目依赖)
- gte-small ONNX 模型文件 (~130MB，首次启动下载或随 App 分发)

### Phase 2: 扩展 — kb_search（预估 2-3 天）

```
□ internal/rag/kb_importer.go    — Markdown 知识摄入管道
□ internal/rag/kb_tool.go        — kb_search 工具注册
□ internal/rag/skills_bridge.go  — Skills 目录自动索引
□ cmd/codeagent/kb.go            — `codeagent kb import` CLI
```

### Phase 3: 可选 — 云端 embedding（预估 1 天）

```
□ internal/rag/cloud_embedder.go — OpenAI/DeepSeek embedding API
□ 自动降级: 云端不可用时 fallback 本地
```

---

## 8. 端侧与服务端的使用场景

### 核心原则：索引是重的，查询是轻的

```
                     Mac / Server                     iOS / 轻客户端
                     ────────────                     ──────────────

embedding 索引 (写)    ✅ 全量                           ❌ 不做
                      - Tree-sitter AST 解析              （资源受限 + 沙盒）
                      - chunking + embedding
                      - 1 万文件 ~30 秒

embedding 查询 (读)    ✅                               ✅
                      - SQLite + vec 扩展                - 同一份 SQLite 文件
                      - < 50ms/query                    - < 50ms/query
                      - 130MB 模型                       - 130MB 模型

知识管理               ✅                                ❌
                      - CLI: kb import ./notes/          （接收成品索引）
                      - IDE 插件
                      - Skills 目录自动索引
```

索引文件（`kb_index.db` + `code_index.db`）是普通的 SQLite 数据库，可以像文件一样分发——iCloud、git-lfs、对象存储、App Bundle 预置。

### 8.1 iOS 端 RAG 的价值重定义

Mac 端和 iOS 端的用户行为完全不同：

```
Mac 端:  用户正在写代码 → code_search 搜代码库找实现
iOS 端:  用户正在学知识 → kb_search 搜知识库找答案
```

iOS 上 code-agent 跑在 sandboxed 模式——没有 shell、没有 git、没有编译器。用户不是在写代码，是在**学代码、查资料、理解概念**。这决定了 RAG 在两端的主角不同：

| | Mac 端 | iOS 端 |
|---|---|---|
| **主力工具** | code_search（代码语义搜索） | kb_search（知识库搜索） |
| **用户行为** | 写代码，搜实现 | 学知识，查资料 |
| **工具环境** | 完整 (shell/git/LSP) | 沙盒 (只读工具) |
| **索引来源** | 当前项目 + 外部知识 | 仅外部知识 (Mac 端预构建) |
| **隐私要求** | 普通 | 极高 (健康/财务/笔记/学习记录) |
| **网络依赖** | 可选 | 期望离线 |

**iOS 端 RAG 的真实场景：**

```
场景 A: 学习助手
  用户: "block 循环引用到底怎么产生的？"
  → kb_search → blocks.md 讲三种场景 → 模型用检索到的知识回答
  → 地铁上离线可用

场景 B: API 速查
  用户: "UIView animateWithDuration 的完整参数？"
  → kb_search → UIKit 文档在知识库里 → 返回签名 + 示例
  → 不用打开 Safari 搜 StackOverflow

场景 C: 面试准备
  用户: "给我出一道 weak vs strong 的面试题"
  → kb_search → 面试题库 → 出题 + 评判答案

场景 D: 隐私文档问答（云端 RAG 做不到的）
  用户: "上个月的血压数据有什么异常？"
  → kb_search → 健康数据导出在本地 → Agent 分析
  → 数据绝不离开设备，HIPAA 合规
```

**结论**：RAG 在 iOS 上不是"搜代码"，是**带着私有知识回答问题**。这是端侧 RAG 相比云端 RAG 的不可替代优势——隐私数据永不离开设备。

### 场景 1：个人开发者（Mac 索引 → iOS 消费）

```
┌─ Mac（索引端）─────────────────────────────────────────────┐
│                                                             │
│  # 导入学习资料                                             │
│  codeagent kb import ./objc-notes/                          │
│  codeagent kb import-url https://developer.apple.com/...    │
│                                                             │
│  # 索引当前项目代码                                         │
│  codeagent code index                                       │
│                                                             │
│  产出:                                                      │
│    .codeagent/rag/kb_index.db    (知识库向量索引)            │
│    .codeagent/rag/code_index.db  (代码语义索引)              │
│                                                             │
└────────────────────┬────────────────────────────────────────┘
                     │
                     │ iCloud Drive / AirDrop / git
                     ▼
┌─ iOS（消费端）─────────────────────────────────────────────┐
│                                                             │
│  AgentKit 启动 code-agent runtime:                          │
│    MobileStart(                                              │
│      workspaceDir:  Documents/,                             │
│      dataDir:       Application Support/,                   │
│      ragIndexPath:  Documents/.codeagent/rag/  ← 索引目录   │
│    )                                                        │
│                                                             │
│  Agent 对话中:                                               │
│    User: "ObjC 的 block 循环引用怎么解决？"                   │
│    → kb_search("block retain cycle weak strong")            │
│    → 本地 embedding，< 100ms，零网络                         │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### 场景 2：团队协作（Server 构建 → 多端拉取）

```
┌─ CI / Server（索引端）────────────────────────────────────┐
│                                                             │
│  定时任务 (每天 / 每次文档更新):                              │
│    codeagent kb import ./team-conventions/                  │
│    codeagent kb import ./api-docs/                          │
│    codeagent code index --workspace ./monorepo/             │
│                                                             │
│  产出: kb_index.db + code_index.db                          │
│  附带: version.txt (时间戳 + 内容 hash)                     │
│  上传: 对象存储 (S3/OSS) / git-lfs / 内部文件服务器          │
│                                                             │
└────────────────────┬────────────────────────────────────────┘
                     │
                     │ HTTP 下载 (启动时检查版本 → 按需更新)
                     ▼
┌─ 各客户端（Mac / iOS / Web）───────────────────────────────┐
│                                                             │
│  启动时:                                                     │
│    GET /rag/version → 比对本地版本                           │
│    有新版本 → 下载 kb_index.db (增量或全量)                  │
│                                                             │
│  Agent 对话中: 查询本地索引 → < 100ms                        │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

**版本更新策略**：

```
本地: .codeagent/rag/version.txt
  → 2026-06-30T12:00:00Z  sha256:abc123

服务端: GET https://team-server/rag/version
  → 2026-07-01T09:00:00Z  sha256:def456

版本不一致 → 下载新索引 → 原子替换 → 无需重启
```

### 场景 3：纯离线 iOS（App Bundle 预置知识库）

```
App 打包时:
  ├── CodeAgentRuntime.xcframework
  ├── Models/
  │   └── gte-small.onnx                 ← 嵌入模型 (130MB)
  └── Assets/
      └── default_kb_index.db            ← 预置知识库索引
          (Apple 官方文档 / 团队规范 / 学习资料)

首次启动:
  code-agent 检测到 ragIndexPath 下有索引文件
    → 加载 kb_index.db
    → kb_search 立即可用
    → 后续可通过 Mac 同步或 OTA 更新索引
```

App Store 分发注意：嵌入模型 + 预置索引约 200-500MB，需评估包体积影响。可选择首次启动时下载（On-Demand Resources）。

### 与 Skills 的联动

Skills 是静态指令，RAG 索引是动态知识。两者在两端的分工不同：

```
Mac 端 (管理):
  skills/objc-learning/
  ├── SKILL.md              ← 始终加载的 system prompt 片段
  └── knowledge/             ← 目录存在 → codeagent kb import 自动索引
      ├── blocks.md          → 分块 → embedding → kb_index.db
      ├── runtime.md
      └── arc.md

  codeagent kb import skills/objc-learning/knowledge/
  → knowledge/ 下所有 .md 自动分块 → embedding → 写入 kb_index.db

iOS 端 (消费):
  - SKILL.md 通过 configYAML 或代码注入 system prompt
  - kb_index.db 随 iCloud/OTA 同步 → kb_search 立即可用
  - 用户不需要管理任何文件
```

### 对 code-agent Runtime 接口的改动

```go
// mobile/mobile.go — 新增 ragIndexPath 参数
func Start(
    workspaceDir, dataDir string,
    configYAML, modelName, secretsJSON string,
    addr string,
    sandboxed bool,
    ragIndexPath string,  // ← 新增：RAG 索引目录，空字符串 = 不启用
) (*Server, error)
```

```go
// internal/embed/server.go — StartServer 中装配 RAG 工具
if opt.RagIndexPath != "" {
    ragEngine, err := rag.Load(opt.RagIndexPath)
    if err == nil {
        registry.Register(rag.NewCodeSearchTool(ragEngine))
        registry.Register(rag.NewKBSearchTool(ragEngine))
    }
}
```

**零开销原则**：`ragIndexPath` 为空时，RAG 引擎完全不加载，工具不注册，内存和启动时间不受影响。

---

## 与行业方案的差异总结

| | Cursor | Copilot | Claude Code | **code-agent** |
|---|---|---|---|---|
| 语义代码搜索 | ✅ | ✅ | ❌ | ✅ **Phase 1** |
| 外部知识库 | ❌ | ❌ | ✅ 静态 | ✅ **Phase 2 动态** |
| 嵌入存储 | 云端 | 混合 | N/A | **全本地** |
| 检索模式 | Agentic | 预计算 + Agentic | N/A | **Agentic** |
| 隐私 | 向量在云端 | 混合 | 全本地 | **全本地** |
| 离线 | ❌ | ❌ | ✅ | ✅ |
| 增量索引 | Merkle tree | 秒级重索引 | N/A | **mtime 比对** |
