# code-agent RAG 功能设计

> 基于 Claude Code / Cursor / GitHub Copilot 的 RAG 实践调研
> 日期: 2026-06-30

## 目录

1. [行业调研](#1-行业调研)
2. [核心设计决策](#2-核心设计决策)
3. [架构设计](#3-架构设计)
4. [code_search — 代码语义搜索（Phase 2，实验性）](#4-code_search--代码语义搜索phase-2实验性)
5. [kb_search — 外部知识库（Phase 1）](#5-kb_search--外部知识库phase-1)
6. [嵌入运行时选型](#6-嵌入运行时选型)
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
  ✅ 纯 Go BLOB + 暴力 cosine，无外部服务依赖（详见决策 5，不使用 vec0）
  ⚠️ 云端 embedding API 作为可选项 (Phase 3, 后期)
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
选择：Markdown 标题分块 (Phase 1)，AST-aware 分块 (Phase 2 实验)

理由:
  ✅ Markdown 按 ## 标题天然分块，零依赖，Phase 1 立即可做
  ✅ AST 分块是行业最佳实践（Copilot、Cursor），但需引入 tree-sitter
  ⚠️ tree-sitter 非项目已有依赖，且多语言语法文件增加构建复杂度
  ⚠️ project_graph 用的是 gopls (LSP)，不可复用其"AST 解析能力"
  → Phase 2 引入 tree-sitter 前需独立评估 ROI
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

### 决策 5：向量存储 — 纯 Go 暴力 cosine vs ANN 扩展

```
选择：纯 Go BLOB + 暴力 cosine (Phase 1)，需要 ANN 时再引入

理由:
  ✅ 384d × 几万条 chunk → 点积 < 10ms，对 Agent 场景完全够用
  ✅ 零外部依赖，项目现用 modernc.org/sqlite (纯 Go, 无 CGo)
  ❌ sqlite-vec (vec0 虚拟表) 是 C 加载式扩展，与纯 Go SQLite 不兼容
  ❌ 若改用 mattn/go-sqlite3 (CGo) 只是为了加载 vec0：
     → 引入 CGo → 冲击 gomobile/iOS xcframework 构建
     → ROI 极低（几万条规模下 ANN 收益微乎其微）
  → 需要扩展到 10 万+ 条时，优先评估纯 Go 的 HNSW (如 usearch-go)
```

### 决策 6：增量索引 — 内容 hash vs mtime

```
选择：内容 hash (SHA-256) + mtime 作为快速跳过提示

理由:
  ✅ mtime 在 git checkout/rebase/stash pop 后会被重置，不可信赖
  ✅ Cursor 用 Merkle tree 正是为此——本质上就是内容 hash
  ✅ 增量流程: 遍历文件 → 对比 hash → 仅重索引变更文件
  ✅ 同时处理文件删除/重命名 → 清理陈旧向量，防止索引腐烂
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
│  │              │  │              │  │  Phase 1:      │  │
│  │  Core ML     │  │  SQLite BLOB │  │  Markdown 标题 │  │
│  │  (iOS) 或    │  │  + 纯 Go     │  │               │  │
│  │  ONNX (Mac)  │  │  暴力 cosine │  │  Phase 2:      │  │
│  │  ~130 MB     │  │  零外部依赖   │  │  tree-sitter   │  │
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
│  增量更新: 内容 hash (SHA-256) 比对，仅重索引变更文件      │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │  端侧 (Mac/iOS): 全部本地，零网络，零隐私风险       │   │
│  │  云端 fallback (可选 Phase 3): embedding API      │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 组件清单

| 组件 | 技术选型 | 大小/性能 | 依赖 | 备注 |
|------|---------|----------|------|------|
| 嵌入模型 | gte-small (Mac: ONNX, iOS: Core ML) | 130 MB, < 50ms/chunk | ONNX runtime 或 Core ML | 需同时解决 tokenizer (WordPiece) |
| 向量存储 | SQLite BLOB + 纯 Go 暴力 cosine | < 10ms for 10k chunks | 零外部依赖 (modernc.org/sqlite) | 项目现用纯 Go SQLite，无 CGo |
| 分块 (Phase 1) | Markdown 按 `##` 标题 | 零依赖 | 标准库 | kb_search 主力 |
| 分块 (Phase 2) | Tree-sitter AST | 多语言语法文件 | smacker/go-tree-sitter (新引入) | code_search 实验 |
| 增量索引 | 内容 hash (SHA-256) + mtime 快速跳过 | O(changed) | 标准库 | mtime 仅作跳过提示，不以它为准 |
| 陈旧清理 | 遍历索引 → 对比文件系统 → 删除不存在文件的向量 | O(chunks) | 标准库 | 防止索引腐烂 |

---

## 4. code_search — 代码语义搜索（Phase 2，实验性）

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
    ▼ 增量更新: 对比每个文件的内容 hash，仅重索引变更文件
```

### 查询流程

```
用户/模型调用 code_search(query="内存管理核心逻辑", top_k=5)
    │
    ▼ query → gte-small → query_embedding (384d)
    │
    ▼ 纯 Go: 读取 namespace='code' 的全部 embedding BLOB
    │         → 内存中逐条计算 cosine 相似度 → 排序取 top_k
    │         （无 SQL 向量扩展，详见决策 5）
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

### Hybrid 检索策略

2025-2026 的 hybrid 共识是 dense + sparse 双路召回 → RRF 融合，而非单路 + 阈值降级。

```
用户查询 → ┌─ dense embedding → cosine top-20 ─┐
           └─ sparse BM25 tokens → BM25 top-20 ─┘
                        │
                        ▼
                  RRF (Reciprocal Rank Fusion) 融合排序
                        │
                        ▼
                  top-20 → Reranker → top-5 返回给 Agent
```

**Reranker 方案（按优先级）**：

1. **Claude 自身做 reranker**（Phase 1 首选）：召回 top-20 后，把候选项编号，让 Agent 在 loop 中评估相关性。Agentic 架构天然支持。
2. **轻量 cross-encoder**（Phase 3 可选）：如 `ms-marco-MiniLM` 转为 Core ML，本地精排。

**不硬编码 cosine 阈值**：阈值的绝对值跨模型、跨 query 不可移植。用 RRF 的相对排序 + top-k 截断替代。检索失败时，Agent 收到的结果为空，自然会尝试 grep。

---

## 5. kb_search — 外部知识库（Phase 1）

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

## 6. 嵌入运行时选型

### 6.1 平台路线

```
Mac:  ONNX runtime (开发阶段快速验证)
      → 后续迁移到 Core AI (macOS 27+，原生 Apple Silicon 优化)

iOS:  Core ML (与 LLM 推理路线对齐)
      → gte-small 转为 Core ML 模型 → ANE 推理
      → 与 ios-on-device-llm-feasibility.md 的端侧 runtime 选型一致
      → 避免另起 ONNX 增加 gomobile 构建复杂度
```

### 6.2 Embedder 接口

```go
// Embedder 接口 — 本地/云端统一抽象
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
    ModelID() string    // 模型标识，用于版本锁
    Dims() int          // 384 for gte-small
    MaxTokens() int     // 512 for gte-small
    Pooling() string    // "mean" / "cls"
}

// Mac: ONNX (开发阶段)
type ONNXEmbedder struct { ... }

// iOS: Core ML (生产)
type CoreMLEmbedder struct { ... }

// 可选: 云端 API
type CloudEmbedder struct { ... }

func NewEmbedder(cfg RAGConfig) Embedder { ... }
```

### 6.3 Tokenizer（被遗漏的工作量）

gte-small 使用 WordPiece 分词器，ONNX/Core ML 模型本身不含分词器。需要在 Go/Swift 侧实现或内嵌词表。评估方案（按优先级）：

1. **Go 侧内嵌词表**：将 `vocab.txt` (~110KB) 用 `embed.FS` 打包进二进制，用纯 Go 实现 WordPiece。可行性高，可复用 HuggingFace tokenizers 的参考实现。
2. **Swift 侧 NLTokenizer**：iOS 上用 NaturalLanguage 框架的 `NLTokenizer`，但它是 BPE 而非 WordPiece，不完全兼容。
3. **简化方案**：用 Unicode 分段 + 空格分词作为兜底（精度损失约 5-10%，但对 RAG 召回影响可控）。

### 6.4 模型版本锁（索引分发的正确性前提）

query embedding 必须由**与建索引时完全相同的模型 + 版本 + 池化方式**产生，否则向量空间不对齐，相似度全是噪声。`version.txt` 必须包含：

```json
{
  "index_created":  "2026-07-01T09:00:00Z",
  "content_hash":   "sha256:def456...",
  "num_chunks":     12345,
  "embedding_model": {
    "model_id":   "thenlper/gte-small",
    "commit":     "sha256:abc123...",
    "dims":       384,
    "pooling":    "mean",
    "max_tokens": 512
  }
}
```

加载时校验：model_id + dims + pooling 三者全部匹配才接受索引，否则拒绝并提示重建。

### 6.5 超长文本截断（512 token 限制）

gte-small max 512 token。超过此限制的 chunk 需二级切分：

```
函数 ≤ 512 token → 一个 chunk
函数 > 512 token → 按逻辑边界切分：
  1. 按自然段落 + overlap 100 token
  2. 每个子 chunk 前缀拼接函数签名 + 文件路径（上下文增强）
  3. 检索时取同一父函数下的最高分子 chunk
```

### 6.6 上下文增强（Contextual Retrieval）

Anthropic 2024 年 9 月提出的技术：用 LLM 给每个 chunk 生成一句上下文前缀，配合 reranker 可提升召回约 49%。

```
原始 chunk:
  "func dealloc() { _ivar = nil; [super dealloc]; }"

增强后 (LLM 生成前缀 + 原文):
  "此函数是自定义对象的 dealloc 方法，用于释放实例变量并调用父类清理。
   func dealloc() { _ivar = nil; [super dealloc]; }"
```

对 code-agent 场景，可在**索引阶段用 Claude 自身**给重要代码块生成前缀。对于 kb_search 场景（Markdown 文档），章节标题本身已经提供了足够的上下文。

---

## 7. 实现计划

### Phase 0: 评测框架（先于所有功能开发）

没有评测集就无法决定 top_k、阈值、是否换模型、reranker 值不值。

```
□ internal/rag/eval.go           — recall@k / MRR / nDCG 评测框架
□ testdata/rag/                  — 评测数据集
    ├── queries.json              — 自然语言查询 + 期望相关文档 ID
    └── knowledge/                — 知识库语料
□ 基线: BM25 (词汇匹配) vs gte-small (纯 dense)
□ 每次模型/分块/参数变更后重跑评测 → 数据驱动决策
```

### Phase 1: kb_search — 外部知识库（预估 1.5-2 周）

这是 ROI 最高的功能。Markdown 分块零依赖，iOS 端主场，隐私问答是云端做不到的差异化。

```
□ internal/rag/embedder.go       — Embedder 接口 + tokenizer
    ├── onnx_embedder.go          — Mac: ONNX runtime
    └── coreml_embedder.go        — iOS: Core ML
□ internal/rag/chunker.go        — Markdown 按 ## 标题分块 (零依赖)
□ internal/rag/vector_store.go   — SQLite BLOB + 纯 Go 暴力 cosine
    ├── 建表、写入、查询、删除
    └── model version lock 校验
□ internal/rag/indexer.go        — 索引器 (内容 hash 增量 + 陈旧清理)
□ internal/rag/searcher.go       — 语义搜索 + BM25 双路召回 → RRF 融合
□ internal/rag/kb_importer.go    — Markdown 知识摄入管道
□ internal/rag/skills_bridge.go  — Skills knowledge/ 目录自动索引
□ internal/rag/tool.go           — kb_search 工具注册
□ internal/rag/config.go         — RAG 配置
□ cmd/codeagent/kb.go            — `codeagent kb import` CLI
```

**依赖**：
- gte-small 模型文件 (~130MB，首次启动下载或随 App 分发)
- WordPiece 词表 (~110KB，Go `embed.FS` 打包)
- `modernc.org/sqlite` (项目已有，纯 Go)
- 无需 tree-sitter，无需 vec0，无需 CGo

### Phase 2: code_search — 代码语义搜索（实验性，预估 2-3 周 + 高不确定性）

投入前需先做对照实验：*强 agent 的 grep+read_file+gopls* vs *加上 code_search*，用评测集证明语义搜索真的赢。

```
□ internal/rag/chunker_code.go   — Tree-sitter AST 分块 (新引入依赖)
    ├── Go/Swift/Python/... 语言语法文件
    ├── 超长函数二级切分 + overlap
    ├── 上下文增强 (文件路径、签名、docstring)
    └── 解析失败回退: 固定窗口 + overlap
□ internal/rag/contextual.go     — LLM 上下文前缀生成 (Claude 自身)
□ internal/rag/tool_code.go      — code_search 工具注册
```

### Phase 3: 云端 embedding + Reranker（预估 1 周）

```
□ internal/rag/cloud_embedder.go — OpenAI/DeepSeek embedding API
□ internal/rag/reranker.go       — Claude 自身做 reranker (召回 top-20 → Claude 挑 top-5)
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
                      - SQLite BLOB + 纯 Go cosine       - 同一份 SQLite 文件
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
  → 数据永不离开设备（隐私优势，云端 RAG 做不到）
```

**iOS 上 code_index 的硬约束**：code_index 返回的是 `file_path + line_range`，Agent 仍需调用 `read_file` 取源码。iOS 沙盒里没有项目源码时 code_index 无意义。因此 iOS 端只启用 kb_search，code_index 必须与源码仓库在同设备（Mac/Server）上运行。

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

版本不一致 → 关闭当前索引连接 → 下载新索引 → 原子替换 → 重新打开（需 WAL 句柄安全关闭）
```

### 场景 3：纯离线 iOS（App Bundle 预置知识库）

```
App 打包时:
  ├── CodeAgentRuntime.xcframework
  ├── Models/
  │   └── gte-small.mlmodelc             ← 嵌入模型 Core ML (iOS, ~130MB)
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
| 语义代码搜索 | ✅ | ✅ | ❌ | ✅ **Phase 2（实验）** |
| 外部知识库 | ❌ | ❌ | ✅ 静态 | ✅ **Phase 1 动态** |
| 嵌入存储 | 云端 | 混合 | N/A | **全本地** |
| 检索模式 | Agentic | 预计算 + Agentic | N/A | **Agentic** |
| 隐私 | 向量在云端 | 混合 | 全本地 | **全本地** |
| 离线 | ❌ | ❌ | ✅ | ✅ |
| 增量索引 | Merkle tree | 秒级重索引 | N/A | **内容 hash 比对** |
