# iOS 端侧本地大模型可行性分析

> 面向 code-agent iOS xcframework 集成场景
> 日期: 2026-06-30

## 目录

1. [硬约束：iOS 内存模型](#1-硬约束ios-内存模型)
2. [模型选型](#2-模型选型)
3. [推理运行时对比](#3-推理运行时对比)
4. [集成路径分析](#4-集成路径分析)
5. [使用场景评估](#5-使用场景评估)
   - [5.1 场景分类](#5-使用场景评估)
   - [5.2 现实检验：本地模型在 Agent 场景的真实表现](#52-现实检验本地模型在-agent-场景的真实表现)
6. [WWDC 2026 想象空间](#6-wwdc-2026-想象空间)
7. [结论与建议](#7-结论与建议)

---

## 1. 硬约束：iOS 内存模型

iOS 的 Jetsam 机制比 macOS 严格得多——超内存直接杀进程，无 swap。

### 各设备内存上限

| 设备 | 物理 RAM | Metal 工作集上限 | 可用于模型 |
|------|---------|-----------------|-----------|
| iPhone 12–13 | 4 GB | ~2.5 GB | ~1.5 GB |
| iPhone 14–15 | 6 GB | ~4 GB | ~2.5 GB |
| **iPhone 15/16 Pro** | **8 GB** | **~5.5 GB** | **3–4 GB** |
| iPad Pro M4 | 8–16 GB | ~7–12 GB | 4–8 GB |
| iPhone 17 Pro | 12 GB | ~8 GB | 5–8 GB |

> **经验法则**：模型文件大小 × 1.2–1.5 ≈ 运行时内存占用（含 KV Cache 和上下文缓冲区）

### code-agent runtime 自身开销

Go 运行时 + session SQLite + 工具注册表 + WebSocket 服务约占用 200–400 MB。在 8 GB 设备上，留给模型的可用内存约 **3–4 GB**。

---

## 2. 模型选型

对 code-agent 场景，模型必须同时满足：**函数调用 (tool calling) + 代码理解 + 多轮对话**。

### 候选模型

| 模型 | 大小 (Q4_K_M) | 函数调用 | 代码能力 | 适合设备 |
|------|-------------|---------|---------|---------|
| **Hammer 2.1 3B** | ~2.0 GB | ⭐⭐⭐⭐⭐ 专为 FC 设计 | ⭐⭐⭐ | iPhone 15 Pro+ |
| **Qwen 2.5 Coder 3B** | ~2.0 GB | ⭐⭐⭐ 需模板 | ⭐⭐⭐⭐ | iPhone 15 Pro+ |
| **Apple Foundation Model** | ~1.8 GB (系统共享) | ⭐⭐⭐ 原生 Tool 协议 | ⭐⭐ | A17 Pro+, iOS 26 |
| Hammer 2.1 1.5B | ~1.0 GB | ⭐⭐⭐⭐ | ⭐⭐ | iPhone 14+ |
| Phi-4 Mini 4B | ~2.5 GB | ⭐⭐⭐ | ⭐⭐⭐⭐ | iPad M4 / 17 Pro |
| Qwen 2.5 Coder 7B | ~4.5 GB | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ | Mac only |
| Qwen 3 Coder 30B MoE | ~18 GB | ⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐ | Mac (32GB+) |

### 关键限制

- **3B 级别的代码模型**能处理简单任务（读文件、搜索、小改动），但复杂多步推理能力显著不足
- **Apple Foundation Model** 上下文窗口仅 4,096 token——对 agent 场景严重不足（项目快照 + 工具定义就超了）
- **Hammer 2.1** 是专门为函数调用优化的，Berkeley FC Leaderboard 上排名靠前，是端侧 agent 的最佳基座选择

### macOS 实测经验

在 M1 Pro 32GB 上跑 qwen3-coder (30B MoE, 18GB)：

- 首次模型加载：10–15 秒
- keep_alive 后续调用：0.2 秒
- 推理速度：15–20 tok/s
- 中文对话正常，但工具调用需要自定义 Ollama 模板修复（见 `ollama-qwen3-coder.Modelfile`）

这个 18GB 的模型在 M1 Pro 上已经能用的前提下，仍然频繁出现工具调用格式错误和不完整回复。**端侧 3B 模型的能力天花板可想而知**。

---

## 3. 推理运行时对比

### iPhone 17 Pro (A19 Pro) 实测 (2B 模型)

| 运行时 | 解码速度 | 峰值内存 | 10 分钟后速度保持率 | 集成方式 |
|--------|---------|---------|-------------------|---------|
| **MLX Swift** | **61 tok/s** | 1,279 MB | 38% | Swift 进程内 |
| **llama.cpp** | 39 tok/s | 1,479 MB | 50% | C++ / CGo |
| **CoreML/ANE** | 28 tok/s | **241 MB** | **67%** | Swift 封装 |
| LiteRT-LM (Gemma) | 55 tok/s | 641 MB | 48% | Google 框架 |
| Apple Foundation | 30 tok/s | 系统管理 | 极低 | iOS 26 原生 |

### 关键发现

- **MLX Swift 最快**，但持续负载下热降频严重（损失 62% 性能）
- **CoreML/ANE 最省电**，功耗仅为 GPU 的一半，适合常驻后台
- **ANE 几乎不降频**——这是 agent 长时间运行的关键优势
- **llama.cpp GGUF 生态最成熟**，跨平台一致，但内存效率不如 MLX

> 对 agent 场景的启示：agent 是多轮持续负载。CoreML/ANE 的散热优势可能比峰值速度更重要。

---

## 4. 集成路径分析

当前 code-agent iOS 架构是：

```
gomobile bind → xcframework → Swift 进程内启动 Go runtime → HTTP/WS loopback
```

### 路径 A：llama.cpp CGo 嵌入

```
Go provider "llamacpp" → CGo → llama.cpp → Metal GPU
```

| 优点 | 缺点 |
|------|------|
| 同进程调用，无 IPC 开销 | CGo 增加 xcframework 构建复杂度 |
| GGUF 生态成熟，模型丰富 | 内存效率不如 MLX/CoreML |
| 可复用 macOS 上的 provider 模式 | A17 Pro 上约 10–15 tok/s (3B) |

### 路径 B：独立推理进程

```
Swift 侧 MLX/llama.cpp server → Go provider localhost HTTP
```

| 优点 | 缺点 |
|------|------|
| 可选 MLX (最快) 或 CoreML (最省电) | iOS 后台进程限制严格 |
| 推理引擎独立演进 | IPC 开销 + 两进程内存占用 |

### 路径 C：iOS 26 Foundation Models（WWDC 2026 后）

```
Swift Tool 协议 → LanguageModel → System / PCC / CoreAI
```

| 优点 | 缺点 |
|------|------|
| 零模型下载，系统共享 | **仅 4K 上下文**，无法跑 agent |
| `LanguageModel` 协议统一所有提供方 | Tool 协议与 OpenAI tool_calls 不兼容，需适配层 |
| PCC 32K 推理免费（< 200 万下载） | 仅 iOS 26+ |

### 路径 D：Core AI 自定义模型（WWDC 2026 后）

```
微调模型 (PyTorch) → coreai-torch 转换 → Core AI → ANE/GPU
```

| 优点 | 缺点 |
|------|------|
| 可部署专用 CodeAgent 模型 | 需要微调数据和工程投入 |
| ANE 推理极低功耗 | 仅 iOS 26+ |
| Swift 原生 API，内存安全 | 模型分发策略待定 |

---

## 5. 使用场景评估

### 适合本地模型的场景

| 场景 | 适合度 | 理由 |
|------|-------|------|
| **隐私敏感的代码分析** | ✅ 最佳 | 本地代码审查、密钥检测、敏感文档分析 |
| **离线基础编码辅助** | ✅ | 无网络下的文件搜索、项目结构分析 |
| **意图分类与路由** | ✅ 最佳 | 小模型判断"该调哪个工具/模型"，重活交云端 |
| **简单单步工具调用** | ✅ | 读文件、grep、项目图查询 |
| **常驻后台守护** | ✅ | 代码变更监控、安全检查 (ANE 几乎不发热) |

### 不适合本地模型的场景

| 场景 | 适合度 | 理由 |
|------|-------|------|
| **复杂多步推理** | ❌ | 3B 模型缺乏深度规划能力 |
| **大型重构** | ❌ | 上下文和理解深度都不够 |
| **长上下文任务** | ❌ | 端侧模型上下文窗口 4K–32K |
| **需要前沿模型的任务** | ❌ | 3B 永远追不上 100B+ |

### 最佳实践：混合模式

```
iOS App
  │
  ├── 本地 3B 模型 (端侧)
  │   ├── 意图分类
  │   ├── 简单工具执行
  │   ├── 隐私敏感操作
  │   └── 路由决策
  │
  └── 云端大模型 (deepseek / claude / PCC)
      ├── 复杂推理 + 多步规划
      ├── 大范围重构
      └── 长上下文分析
```

这与 code-agent 的 `subagent_model` 机制天然契合——本地小模型做 subagent 的只读搜索，主模型走云端。

### 5.2 现实检验：本地模型在 Agent 场景的真实表现

以下数据来自 code-agent 在 M1 Pro 32GB 上实测 qwen3-coder (30B MoE, 18GB, Q4_K_M) vs deepseek-v4-flash 的对比：

| 维度 | qwen3-coder (本地 18GB) | deepseek-v4-flash (云端) |
|------|------------------------|------------------------|
| 激活参数 | 3.3B (MoE) | 估计 100B+ |
| 工具调用成功率 | ~60% | ~99% |
| 理解工具 schema | ❌ 经常传空值 | ✅ 严格按参数要求 |
| 多步推理 (先读后写) | ❌ 跳过 read 直接 write/edit | ✅ 自动 read→edit |
| 从错误中恢复 | ❌ 重复相同错误 | ✅ 自动换策略 |
| 上下文利用 | ❌ observation 看了就忘 | ✅ 全吸收 |
| 单步耗时 | 10–50s | 1–3s |
| 需要人工干预 | 频繁 | 几乎没有 |
| 硬件成本 | 18GB 内存 + GPU 满载 | 0 |
| 持续负载功耗 | ~30W (风扇满转) | 0 (本地仅 HTTP) |

**实测失败案例：**

1. **`edit_file` 的 `old` 参数为空** — 模型调用 `edit_file` 但传 `old: ""`。它不理解需要先从文件里读出原文再替换，直接编了一个空参数。Observation 返回了明确错误提示，下一轮它依然传空。

2. **重复使用 `create_file`** — 第一次失败，observation 告知"文件已存在，请用 edit_file"。几秒后模型再次调 `create_file` 到同一路径，得到相同错误。上下文利用能力几乎为零。

3. **497 秒无效循环** — 模型在一个任务上思考 497 秒，反复尝试相同策略，每次都失败。云端大模型会换思路，本地模型只会原地打转。

**根因：**

不是调参能解决的。30B MoE 不代表有 30B 的推理能力——只有 3.3B 参数真正工作。Agent 是对模型能力要求最高的场景（不是之一），需要同时具备精确工具调用、多步推理、上下文吸收、错误恢复、长程规划。这五条，3B 级模型一条都做不到。

**本地模型正确的定位：**

把 LLM 想象成引擎。Agent 需要 V8 涡轮增压，本地模型给的是单缸摩托车发动机。但单缸发动机也有它该去的地方：

| 场景 | 适合本地 | 理由 |
|------|---------|------|
| **意图路由** | ✅ 最该做 | 只需分类，不需工具调用。3B 延迟 < 100ms |
| **代码检索** | ✅ | embedding 编码 + 匹配。`gte-small` (~130MB) |
| **安全审查** | ✅ 天然优势 | 代码不离开设备，隐私零风险 |
| **单行补全** | ✅ | 不需要深度推理，3B Coder 模型足够 |
| **Agent 主循环** | ❌ 不要做 | 多步推理 + 工具调用，天花板在这里 |

本地模型在 code-agent 里不是替代 deepseek，是在 deepseek 看不见的地方做 deepseek 做不了的事——隐私保护、零延迟路由、离线可用。

---

## 6. WWDC 2026 想象空间

### 6.0 正确的架构边界：Go 做 Agent 引擎，Swift 做模型推理

```
┌─ Swift 层：Apple 平台专属 ─────────────────────────────────────┐
│                                                                │
│  Core AI Model Loader / Cache  (AIModelCache)                   │
│  LanguageModel Provider  (System / PCC / CoreAI / MLX)          │
│  Dynamic Profiles  (本地 3B vs PCC 路由决策)                     │
│  @Generable Tool Schema  (编译期 schema 生成)                    │
│  ANE/GPU 调度  (热降频管理)                                      │
│                                                                │
│  ┌─ Go 层：跨平台 Agent Engine ──────────────────────────┐    │
│  │                                                        │    │
│  │  Agent Loop  (plan → execute → reflect)                │    │
│  │  Tool Registry  (shell, git, grep, MCP, flux)          │    │
│  │  Session Store  (SQLite + compaction)                  │    │
│  │  Agent-Wire WebSocket Server                           │    │
│  │  Observer / Telemetry / Cost Tracking                  │    │
│  │                                                        │    │
│  │  ┌──────────────────────────────────────────┐         │    │
│  │  │ AppleModelProvider (实现 Provider 接口)    │         │    │
│  │  │ ── FFI ──► Swift LanguageModelSession    │         │    │
│  │  └──────────────────────────────────────────┘         │    │
│  └────────────────────────────────────────────────────────┘    │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

**Go 层保持不动**——Agent 引擎、工具链、会话管理这些是跨平台的核心资产。**Swift 层只做一件事**——把 Apple 的模型能力封装成 Go 的 `Provider` 接口。边界在 `Provider` 接口上，这是我们从一开始就设计好的扩展点，Apple 的 `LanguageModel` 协议选择了同样的抽象层次，说明这个边界是对的。

### 6.1 Core AI — 替代 llama.cpp 的官方推理引擎

Apple 全新的端侧 AI 推理框架，定位为"Core ML 的 LLM 继任者"：

```
Core ML: 通用 ML 模型，不适配 LLM 的 token 生成特性
Core AI: 专为 LLM/VLM 设计，KV-cache 状态管理，AOT 编译
```

关键能力：
- **Swift 原生 API** (`AIModel`, `InferenceFunction`, `NDArray`)
- **PyTorch → Core AI 转换工具链** (`coreai-torch` Python 包)
- **CPU + GPU + ANE 自动调度**
- **AOT 编译**消除首次启动的模型编译延迟
- **Instruments 集成**调试 tensor 回源 Python 代码
- **AIModelCache** 跨 app group 共享模型

对 code-agent 的意义：不再需要 CGo + llama.cpp hack。用 Swift 封装 `CoreAIProvider`，功耗和散热交给系统。

### 6.2 Foundation Models 框架重建 — LanguageModel 协议

WWDC 2026 最核心的架构变化：

```
                    LanguageModel 协议 (统一入口)
                    ╱     │      │     ╲
         System    PCC    CoreAI   MLX   Third-party
        (3B 端侧) (32K 云端) (自定义) (HF) (Claude/Gemini)
```

#### Private Cloud Compute (PCC)

- Apple 的服务器模型：**32K 上下文**，3 档推理深度 (Light/Moderate/Deep)
- 下载量 < 200 万的 App **免费使用**，无 API key
- 数据隐私有密码学保证（Apple 的可信计算边界）
- 对 code-agent：一个 `PCCProvider` 实现，零成本云端推理

#### Dynamic Profiles — OS 级 Agent 框架

```swift
struct AgentProfile: LanguageModelSession.DynamicProfile {
    var body: some DynamicProfile {
        switch states.mode {
        case .quickTask:
            Profile {
                Instructions { "快速完成简单任务" }
                ReadFileTool(), GrepTool()
            }
            .model(.systemOnDevice)

        case .deepReasoning:
            Profile {
                Instructions { "深度分析项目架构..." }
                PlanWorkflowTool(), ShellTool(), GitTool()
            }
            .model(.privateCloudCompute)
            .reasoningLevel(.deep)
        }
    }
}
```

本质上就是**内置的模型路由器 + 工具编排器**。模型自己可以通过 `SwitchModeTool` 在 profile 间切换。

#### Tool 协议 + @Generable

- 编译期从 Swift struct 生成 tool schema（类似 JSON Schema，但类型安全）
- 约束解码（模型只能输出合法参数）
- 与 OpenAI `tool_calls` 不兼容，需要适配层，但**不会出现我们 Ollama 上遇到的格式错误**

### 6.3 开源

Foundation Models 框架 **2026 年夏天开源**，可在 Linux 上运行。这意味着 code-agent 的 macOS/Linux 版本也能直接使用 `LanguageModel` 协议和 PCC。

### 6.4 想象空间总结

| 能力 | 之前 | WWDC 2026 后 |
|------|------|-------------|
| 端侧推理引擎 | llama.cpp CGo hack | Core AI (系统原生) |
| 云端推理 | API key + 按量付费 | PCC (免费, 32K) |
| 模型切换 | 手动 `SelectModel` | Dynamic Profiles (OS 原生) |
| 工具调用格式 | JSON Schema, 模型常出错 | @Generable 约束解码 |
| 模型分发 | 自己下载/打包 GGUF | 系统共享 / Core AI cache |
| 隐私 | 云端 API 数据出站 | PCC 可信计算边界内 |
| 功耗控制 | 手动管理 | ANE 自动调度，几乎不发热 |

Apple 做的事情是把模型推理变成了 **OS 内置的水电煤**。code-agent 的机会不是"取代"，而是**降本增效**——省掉模型对接的脏活，专注做最好的编程 Agent 工具链和体验。

---

## 7. 结论与建议

### 当前 (2026.06)：可以做但价值有限

- 3B 模型的能力天花板很低，工具调用和代码理解都远不如云端模型
- macOS 上 qwen3-coder (18GB, 30B MoE) 都还存在工具调用格式问题
- 投入 CGo + llama.cpp 的工程复杂度与收益不成正比
- **建议**：暂时保持 code-agent 的云端模型路径，利用 `subagent_model` 做模型分层

### WWDC 2026 后：三个最有价值的投入方向

1. **PCC Provider** — 最低成本获得免费云端推理能力
   - 实现 `AppleLanguageModel` → code-agent `Provider` 接口
   - 零 API key，隐私保证，32K 上下文

2. **Dynamic Profiles 适配** — 原生多模型路由
   - 本地 3B 做意图分类 + 简单搜索
   - PCC 做代码分析 + 规划
   - 第三方 (Claude/Gemini) 做超复杂推理

3. **Core AI 专用 CodeAgent 模型** — 长期壁垒
   - 基于 Hammer 2.1 或 Qwen 微调
   - 用 code-agent 的 tool call 格式训练
   - 部署在 ANE 上做常驻代码守护

### 不动

- ❌ 不要在 2026 年内投入 CGo + llama.cpp 到 xcframework
- ❌ 不要期望 3B 模型能替代云端大模型做主 agent
- ✅ 等 Foundation Models 开源后评估 Linux 支持
