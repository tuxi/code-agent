# code-agent MCP 工作空间隔离方案

## 1. 问题总结

### 1.1 核心问题

`codeagentd` 守护进程在启动时（而非按会话/工作空间启动时）一次性加载 `.mcp.json`，导致：

1. **MCP 工具跨会话泄露**：守护进程所在目录的 `.mcp.json` 中定义的所有 MCP 服务器工具对所有会话可见，无论会话的实际工作空间是什么。
2. **会话级 `.mcp.json` 被忽略**：会话在其 `workspace_path` 下配置的 `.mcp.json` 永远不会被加载。
3. **多工作空间隔离不可行**：同一守护进程无法同时服务多个拥有不同 MCP 配置的工作空间。

### 1.2 根因调用链

```
codeagentd 启动 (cmd/codeagentd/main.go)
├── [L53] app.LoadConfig("config.yaml")
│   └── cfg.Workspace.Root 默认为 "."（守护进程 CWD）
│
├── [L55] mcp.ResolveDesktop(cfg.Workspace.Root, ...)
│   └── 加载 <daemon-cwd>/.mcp.json（layering: local > project > user > claude import）
│   └── 返回的 MCP 配置存入 cfg.MCP ← 这是全局唯一的 MCP 配置
│
├── [L98] runtime.BuildRegistry(ctx, cfg, ...)
│   └── 使用 cfg.Workspace.Root 创建技能注册表
│   └── 使用 cfg.MCP 创建 MCP Manager 和工具注册表
│   └── 返回全局唯一的 toolReg、mcpMgr
│
└── 后续所有请求共享此 toolReg / mcpMgr
    └── 会话的 WorkspacePath 仅用于重写 runner.WorkspaceRoot（执行目录）
    └── MCP 工具集从未按工作空间重新解析
```

关键文件与行号：

| 文件 | 行号 | 作用 |
|------|------|------|
| `cmd/codeagentd/main.go` | 53-64 | 启动时一次性 ResolveDesktop |
| `cmd/codeagentd/main.go` | 98 | BuildRegistry 创建全局 toolReg/mcpMgr |
| `internal/runtime/registry.go` | 130-220 | BuildRegistry 使用 cfg.Workspace.Root |
| `internal/runtime/serve_builder.go` | 66-138 | 按请求构建 runner，但复用全局 ToolReg |
| `internal/runtime/workspace.go` | 15-16 | 明确注释 "Tools are NOT workspace-scoped" |
| `internal/mcp/config.go` | 130-158 | ResolveDesktop 从指定 root 加载 .mcp.json |
| `internal/session/session.go` | 26 | Session 有 WorkspacePath，但 MCP 未使用它 |
| `app/config.go` | 365 | WorkspaceConfig{Root: "."} 默认值 |

### 1.3 对比：Claude Code 的做法

Claude Code 在每个工作空间中启动独立进程，因此工作空间自然决定一切：

- `.mcp.json` 相对于打开的项目根目录解析
- `.mcp.local.json` 提供机器级本地覆盖
- `~/.claude.json` 提供用户级服务器定义
- 不同工作空间的 Claude Code 进程拥有独立的 MCP 服务器进程
- 工作空间决定：MCP 配置、权限、CLAUDE.md、技能

`codeagentd` 是多会话单进程架构，因此需要通过显式的按工作空间隔离来实现等价行为。

---

## 2. 方案对比

### 2.1 方案 A：工作空间级 MCP Manager（推荐）

**思路**：将 MCP Manager 与工具注册表一并纳入 `WorkspaceRegistry`，每个工作空间独立管理其 MCP 连接生命周期。

```
WorkspaceRegistry
├── Get(workspacePath) → *Workspace
│   ├── Store          // 已有
│   ├── SkillRegistry  // 已有
│   ├── MCPManager     // 新增
│   └── ToolRegistry   // 新增（或从 MCPManager 派生）
```

**优点**：
- 与现有 `WorkspaceRegistry` 架构一致，扩展而非新建
- 每个工作空间的 MCP 生命周期完全独立：启动、重连、关闭
- 工具集天然隔离，无跨工作空间泄露
- 与技能注册表的按工作空间管理模式一致
- 资源清理明确：工作空间被驱逐时连带 MCP 连接关闭

**缺点**：
- 每个工作空间需启动独立的 MCP 服务器子进程，资源开销增大
- 需要按请求将 `workspacePath` 传递到工具调用链（`ExecutionContext` 已支持）
- 全局 `BuildRegistry` 的语义需要调整（不再创建全局 MCP Manager）
- 启动时间敏感：首个请求到达时需等待 MCP 连接就绪

**适用场景**：多工作空间并发，每个工作空间有独立 MCP 需求。

### 2.2 方案 B：按会话 MCP Manager

**思路**：每个会话（Session）持有自己的 MCP Manager，会话创建时根据 `Session.WorkspacePath` 初始化。

```
Session
├── WorkspacePath
├── MCPManager     // 新增：按会话实例化
└── ...            // 会话结束后销毁
```

**优点**：
- 隔离粒度最细，同一工作空间的不同会话也可有不同 MCP 配置（罕见需求）
- 生命周期与会话绑定，自然跟随会话结束而清理

**缺点**：
- 同一工作空间的多个并发会话会启动重复的 MCP 服务器进程（浪费资源）
- 启动延迟更高：每次新会话都要完成 MCP 握手
- 与会话管理耦合过深，MCP 生命周期与对话生命周期难以分开管理
- 对 `WorkspaceRegistry` 的设计意图（注释说 "Tools are NOT workspace-scoped"）改变更激进
- 热重载 `.mcp.json` 需通知所有活动会话，复杂度高

**适用场景**：会话间需要完全隔离的极端场景。

### 2.3 方案 C：全局 MCP + 覆盖层

**思路**：保留全局 MCP Manager，按工作空间维护一个差异覆盖表。每个工作空间的 `.mcp.json` 与全局配置合并：

```
GlobalMCPManager
├── baseServers: map[name]*MCPServer     // 来自 daemon CWD 的 .mcp.json
├── overlays: map[workspacePath]*Overlay  // 每个工作空间的差异
│   ├── add: map[name]*MCPServerConfig
│   ├── remove: set[name]
│   └── override: map[name]*MCPServerConfig
└── Resolve(workspacePath) → EffectiveConfig
```

**优点**：
- 改动面最小，不改变全局 toolReg 的传递路径
- MCP 服务器进程可跨工作空间共享（同名同配置时）
- 对现有 `BuildRegistry` 语义改动最小

**缺点**：
- 工作空间间工具名冲突难以处理：两个工作空间定义同名但不同实现的 MCP 服务器怎么办
- 工具泄露风险：全局 `toolReg` 需要按请求过滤，每次都要做并集/差集运算
- 覆盖层语义复杂：add/remove/override 的优先级规则容易出错
- MCP 服务器生命周期与工作空间无关，清理时需引用计数
- 与 Claude Code 的用户心智模型（每个工作空间独立）不一致
- `internal/runtime/workspace.go` 注释明确说 Tools 不是 workspace-scoped，方案 C 恰恰回避了修复这个问题

**适用场景**：过渡期方案，或工作空间间 MCP 配置差异极小的场景。

### 2.4 方案对比总结

| 维度 | 方案 A（推荐） | 方案 B | 方案 C |
|------|:---:|:---:|:---:|
| 隔离性 | 高（工作空间级） | 最高（会话级） | 低（并集过滤） |
| 资源效率 | 中 | 低 | 高 |
| 实现复杂度 | 中 | 高 | 低 |
| 与现有架构一致性 | 高（扩展 WorkspaceRegistry） | 中 | 低 |
| 工具泄露风险 | 无 | 无 | 有 |
| Claude Code 对齐 | 是 | 过度隔离 | 否 |
| 热重载支持 | 自然支持 | 需广播 | 需重新计算覆盖 |

---

## 3. 推荐方案详细设计（方案 A）

### 3.1 架构概览

```
┌─────────────────────────────────────────────────┐
│                  codeagentd                       │
│                                                   │
│  ┌──────────────┐       ┌──────────────────────┐ │
│  │ ServeRunBuilder│────▶│ WorkspaceRegistry     │ │
│  │              │       │                      │ │
│  │ workspacePath│       │ Get(path) → Workspace │ │
│  │      │       │       │   ├── MCPManager     │ │
│  │      ▼       │       │   ├── MCPToolRegistry│ │
│  │  Build()     │       │   ├── SkillRegistry  │ │
│  │      │       │       │   └── Store          │ │
│  │      ▼       │       └──────────────────────┘ │
│  │  TurnRunner  │                                 │
│  │   ├── MCP    │                                 │
│  │   │  Tools   │                                 │
│  │   ├── Skills │                                 │
│  │   └── ...    │                                 │
│  └──────────────┘                                 │
└─────────────────────────────────────────────────┘
```

### 3.2 核心变更

#### 3.2.1 `WorkspaceRegistry` 扩展 (`internal/runtime/workspace.go`)

在现有 `Workspace` 结构体中新增 MCP 相关字段：

```go
// Workspace 表示一个已解析的工作空间实例。
type Workspace struct {
    Path      string
    Store     store.Store
    Skills    *skills.Registry

    // 新增：MCP 工作空间级资源
    MCPManager *mcp.Manager     // 按工作空间独立管理 MCP 服务器生命周期
    ToolReg    *tools.Registry  // 从该工作空间的 MCP 配置构建的工具注册表

    mu        sync.RWMutex
    createdAt time.Time
    lastUsed  time.Time
}
```

`Get(workspacePath)` 方法在首次访问时：

1. 解析 `workspacePath/.mcp.json`（通过 `mcp.ResolveDesktop(workspacePath, ...)`）
2. 按配置启动/连接 MCP 服务器，创建 `mcp.Manager`
3. 从 Manager 的 `ListTools()` 构建 `tools.Registry`
4. 缓存到 `Workspace` 实例中

驱逐 (`Evict` / `Prune`) 时，调用 `MCPManager.Close()` 清理 MCP 连接。

#### 3.2.2 `ServeRunBuilder.Build()` 改造 (`internal/runtime/serve_builder.go`)

```go
func (b *ServeRunBuilder) Build(ctx conversation.RuntimeContext) conversation.TurnRunner {
    workspacePath := ctx.Session.WorkspacePath
    // workspace_path 在 conversation 创建时已强制非空（见第 5 节），
    // 此处不存在回退逻辑。

    // 获取工作空间级资源（含 MCP）
    ws := b.WSReg.Get(workspacePath)

    // 技能注册表：按工作空间
    skillReg := ws.Skills

    // MCP 工具注册表：按工作空间（替代全局 b.ToolReg）
    toolReg := ws.ToolReg

    // 合并客户端工具（如前端推送的 MCP 工具）
    if len(ctx.ClientTools) > 0 {
        toolReg = toolReg.Clone()
        toolReg.Merge(clientTools)
    }

    // 其他组件（settings, hooks, approve）同理
    // 见第 7 节

    runner := BuildRunner(
        b.Cfg, mc, provider,
        toolReg,    // ← 现在按工作空间
        skillReg,
        // ...
    )
    runner.WorkspaceRoot = workspacePath
    return runner
}
```

#### 3.2.3 启动流程调整 (`cmd/codeagentd/main.go`)

移除启动时的全局 MCP 初始化：

```go
// 移除这段：
// if cfg.MCP, err = mcp.ResolveDesktop(cfg.Workspace.Root, inheritClaude); err != nil {
//     return err
// }

// BuildRegistry 不再创建全局 MCP Manager
// toolReg 在启动时可为空（或仅包含内置工具）
toolReg, _, mcpMgr, planRef, jobSink, err := runtime.BuildRegistry(ctx, cfg, mc, provider, telemetryStore, nil)
// mcpMgr 返回 nil，启动阶段不连接任何 MCP 服务器
```

或者更彻底地，让 `BuildRegistry` 不再承担 MCP 初始化职责，改为由 `WorkspaceRegistry.Get()` 懒加载。

#### 3.2.4 `ExecutionContext` 已有传递路径

`internal/runtime/runner.go` 中的 `ExecutionContext` 已包含 `WorkspaceRoot`，工具调用时会将工作空间路径传递到工具实现。MCP 工具调用需要通过 `ExecutionContext` 路由到正确的 MCP Manager，而非依赖全局单例。当前路径已就绪。

### 3.3 工具名冲突策略

当多个工作空间定义了同名 MCP 服务器但不同实现时：

- **隔离优先**：每个 `Workspace.ToolReg` 完全独立，不跨工作空间合并
- **会话内冲突**：同一 `.mcp.json` 中定义了同名工具时，`ResolveDesktop` 的 layering 规则已处理（local > project > user）
- **客户端工具合并**：客户端推送的工具与工作空间 MCP 工具合并时，客户端工具覆盖同名 MCP 工具（`tools.Registry.Merge()` 的现有语义）

### 3.4 资源清理与生命周期

```
WorkspaceRegistry
├── Get(path)    → 创建或返回缓存的 Workspace，更新 lastUsed
├── Prune(maxAge) → 驱逐超过 maxAge 未使用的 Workspace
│   └── Workspace.Close()
│       ├── MCPManager.Close()     // 关闭所有 MCP 服务器连接
│       ├── Store.Close()          // 关闭文档存储
│       └── (SkillRegistry 无状态，无需清理)
└── Close()      → 驱逐所有 Workspace
```

驱逐策略：
- `Prune` 由后台 goroutine 定期调用（如每 5 分钟）
- 驱逐条件：`lastUsed` 早于 `maxAge` 且当前无活跃会话引用该工作空间
- 引用计数：每有一个活跃会话使用该 Workspace，`refCount++`；会话结束时 `refCount--`

---

## 4. 问题文档逐项回应

### 4.1 项 A：MCP 工具按工作空间隔离

**实现**：`WorkspaceRegistry` 为每个 `workspacePath` 维护独立的 `MCPManager` 和 `ToolReg`。`ServeRunBuilder.Build()` 使用 `ws.ToolReg` 替代 `b.ToolReg`。

**与 Claude Code 的对齐**：每个工作空间有独立的 MCP 服务器进程，工具集互不可见。

### 4.2 项 B：`.mcp.json` 按会话工作空间加载

**实现**：`WorkspaceRegistry.Get(workspacePath)` 调用 `mcp.ResolveDesktop(workspacePath, ...)` 从会话的 `Session.WorkspacePath` 加载 `.mcp.json`。

**无回退逻辑**：`workspace_path` 在 conversation 创建时强制非空（空则 400，见第 5 节），因此不存在"回退到 daemon 目录"的路径 — 这正是歧义的来源，直接消除。

### 4.3 项 C：`config.yaml.workspace` 直接删除

**决策**：项目未上线，无存量部署需要兼容 — `workspace` 字段（含 `WorkspaceConfig` 结构体）**直接删除**，不走 deprecation 路径。保留一个 deprecated 字段只会延续 `.` 的歧义。

**当前 `workspace.root` 承担的职责与删除后的替代**：

| 使用点 | 现状 | 删除后的替代 |
|--------|------|-------------|
| `codeagentd` MCP 解析 (`main.go:62`) | daemon CWD | 删除 — 移入 WorkspaceRegistry，按 conversation workspace 解析 |
| `codeagentd` telemetry store (`main.go:90`) | daemon CWD 下的 SQLite | `~/.codeagent/daemon/`（daemon 数据目录，与任何项目无关） |
| `codeagentd` 空 workspace_path 回退 (`main.go:104`) | 回退到 daemon CWD | `POST /v1/conversations` 强制要求 `workspace_path`，空则 400 |
| clone endpoint 目标目录 (`main.go:138`) | daemon CWD 子目录 | `~/.codeagent/repos/` 默认值 |
| CLI/TUI (`cmd/codeagent` repl/run) | `cfg.Workspace.Root`（默认 `.`） | `os.Getwd()` — 与 Claude Code 一致：在哪个目录运行，哪个目录就是 workspace |
| `BuildRunner` 内 settings/hooks/verify (`runner.go:48,57,85`) | `cfg.Workspace.Root` | 函数签名加 `root string` 参数，调用方显式传入 |
| `RuleStore` 权限路径 (`serve_builder.go:45`) | daemon CWD | 移入 `WorkspaceInstance`，per-workspace |
| embedded host (`embed/server.go:241`) | `opt.WorkspaceDir` 写入 `cfg.Workspace.Root` | `opt.WorkspaceDir` 作为显式参数传递，不经过 config 中转 |

**入口语义区分**（消除 `.` 的歧义）：

- `codeagent`（CLI/TUI/serve 子命令）：workspace = 进程 CWD。"在项目目录里运行 = 服务这个项目"，与 Claude Code 语义相同。
- `codeagentd`（多 workspace daemon）：**没有默认 workspace**。每个 conversation 必须携带 `workspace_path`（AgentKit 本来就总是传）。daemon 自身数据（telemetry、repos）放 `~/.codeagent/`，与任何项目目录解耦。

**删除的附带收益**：所有隐式读取 `cfg.Workspace.Root` 的代码会编译失败，强制改为显式传参 — 第 7 节的 settings/hooks/permissions 工作空间化从"可选重构"变成"编译器保证的重构"。

### 4.4 项 D：`.mcp.json` 热重载

**实现**：`WorkspaceRegistry` 提供 `Reload(workspacePath)` 方法：

```go
func (wr *WorkspaceRegistry) Reload(workspacePath string) error {
    ws := wr.cache[workspacePath]
    if ws == nil {
        return nil  // 未加载过，无需重载
    }

    // 重新解析 .mcp.json
    newMCP, err := mcp.ResolveDesktop(workspacePath, inheritClaude)
    if err != nil {
        return err
    }

    // 对比新旧 MCP 配置，增量更新
    ws.mu.Lock()
    defer ws.mu.Unlock()

    added, removed, changed := diffServers(ws.currentConfig, newMCP)
    for _, name := range removed {
        ws.MCPManager.StopServer(name)
    }
    for _, name := range changed {
        ws.MCPManager.RestartServer(name, newMCP.Servers[name])
    }
    for _, name := range added {
        ws.MCPManager.StartServer(name, newMCP.Servers[name])
    }

    // 重建工具注册表
    ws.ToolReg = tools.NewRegistry()
    for _, t := range ws.MCPManager.ListTools() {
        ws.ToolReg.Register(t)
    }

    return nil
}
```

**触发机制**：
- API 端点 `POST /v1/workspaces/{path}/mcp/reload`（手动触发）
- `.mcp.json` 文件监听（可选，通过 `fsnotify` 实现，复杂度较高，建议 Phase 2）
- 会话创建时检查文件 mtime，若变化则自动重载（Phase 1）

### 4.5 项 E（额外）：MCP 服务器崩溃与重连

**问题**：MCP 服务器子进程可能崩溃（OOM、panic、被手动 kill）。

**处理**：
- `mcp.Manager` 已有健康检查机制，按需扩展
- SSE/HTTP 服务器：通过定时 ping 检测存活
- stdio 服务器：通过子进程退出信号检测
- 崩溃后自动重启（带指数退避：1s、2s、4s、8s，最大 30s）
- 重启期间工具调用返回明确错误（"MCP server <name> unavailable, reconnecting..."）
- 重启成功后重新 `ListTools` 更新工具注册表

### 4.6 项 F（额外）：启动时 MCP 连接预热

**问题**：`WorkspaceRegistry.Get()` 懒加载意味着首个请求需要等待 MCP 握手完成。

**处理**：
- Phase 1：懒加载，首个请求承担延迟（通常 < 1s，可接受）
- Phase 2：`Init(workspacePath)` 方法允许客户端在发送首条消息前预热 MCP 连接
- Phase 2：`POST /v1/workspaces/{path}/init` API 端点，CLI/前端可主动调用

---

## 5. `config.yaml.workspace` 删除清单

一次性删除（项目未上线，无兼容负担），随 Phase 1 或紧跟其后单独 PR 完成：

### 5.1 代码删除项

| 文件 | 变更 |
|------|------|
| `internal/app/config.go` | 删除 `WorkspaceConfig` 结构体、`Config.Workspace` 字段、`Root: "."` 默认值 |
| `config.yaml` / `config.example.yaml` | 删除 `workspace:` 块 |
| `cmd/codeagentd/main.go` | telemetry store 改开 `~/.codeagent/daemon/`；`NewWorkspaceRegistry` 不再传 defaultRoot；mux `WorkspaceRoot` 改为 `~/.codeagent/repos/` |
| `internal/server/mux.go` | `POST /v1/conversations` 校验 `workspace_path` 非空，空则 400 |
| `internal/runtime/workspace.go` | `WorkspaceRegistry.Get("")` 从"回退 defaultRoot"改为返回错误 |
| `cmd/codeagent/*.go` | 所有 `cfg.Workspace.Root` 替换为 `os.Getwd()`（进程启动时解析一次） |
| `internal/runtime/registry.go` `runner.go` | `BuildRegistry` / `BuildRunner` / `RegisterBuiltinTools` 增加显式 `root string` 参数 |
| `internal/embed/server.go` | `opt.WorkspaceDir` 显式传参，删除对 `cfg.Workspace.Root` 的写入 |

### 5.2 行为变化

- `codeagentd` 从任何目录启动行为完全一致（这正是验收标准第一条）
- 创建 conversation 不带 `workspace_path` → 400（AgentKit 已总是传；CLI serve 子命令默认传自己的 CWD）
- CLI/TUI 行为不变（`workspace.root` 一直默认 `.`，即 CWD）
- daemon 自身数据从"启动目录"迁移到 `~/.codeagent/`：旧的 `.codeagent/` 遗留数据不迁移（未上线，无历史数据需要保留）

---

## 6. 实现计划

### Phase 1：核心隔离（1-2 周）

**目标**：MCP 工具按工作空间隔离，`ServeRunBuilder.Build()` 使用工作空间级 `ToolReg`。

**变更文件**：

| 文件 | 变更 |
|------|------|
| `internal/runtime/workspace.go` | `Workspace` 新增 `MCPManager`、`ToolReg` 字段；`Get()` 方法调用 `mcp.ResolveDesktop` 懒加载；`Close()` 清理 MCP 连接 |
| `internal/runtime/serve_builder.go` | `Build()` 使用 `ws.ToolReg` 替代 `b.ToolReg`；传递 `workspacePath` |
| `internal/runtime/registry.go` | `BuildRegistry` 不再创建全局 MCP Manager，`toolReg` 启动时仅含内置工具 |
| `cmd/codeagentd/main.go` | 移除启动时的 `mcp.ResolveDesktop` 调用；移除 `cfg.MCP` 赋值 |
| `internal/runtime/runner.go` | 确认 `ExecutionContext.WorkspaceRoot` 传递路径（预期无需改动） |

**接口变更**：
- `BuildRegistry` 签名：`mcpMgr` 返回 `nil`（或在签名中去掉）
- `ServeRunBuilder` 不再持有 `ToolReg`（或持有仅含内置工具的基础 ToolReg）

**测试重点**：
- 两个不同工作空间（不同 `.mcp.json`）的并发会话互不干扰
- 工作空间无 `.mcp.json` 时工具集仅含内置工具
- `workspace_path` 为空时 `POST /v1/conversations` 返回 400；`WorkspaceRegistry.Get("")` 返回错误

### Phase 2a：资源管理与重连（1 周）

**目标**：MCP 服务器崩溃自动重连；工作空间驱逐时资源清理。

**变更**：
| 文件 | 变更 |
|------|------|
| `internal/mcp/manager.go` | 新增 `healthCheckLoop`、`reconnectWithBackoff` |
| `internal/runtime/workspace.go` | `Prune()` 方法 + 引用计数；后台 goroutine 定期驱逐 |

### Phase 2b：`.mcp.json` 热重载（1 周）

**目标**：`.mcp.json` 修改后无需重启守护进程即可生效。

**变更**：
| 文件 | 变更 |
|------|------|
| `internal/runtime/workspace.go` | `Reload(workspacePath)` 方法；`Get()` 中结合 mtime 检查 |
| 新增 API | `POST /v1/workspaces/{path}/mcp/reload` |

### Phase 3：设置/权限/技能统一工作空间化（2 周）

**目标**：将与 `workspace.root` 绑定的其他组件也迁移到 `workspacePath`。

**变更**：
| 组件 | 当前路径来源 | 改造后 |
|------|------------|--------|
| `settings.Load` | `cfg.Workspace.Root` | `workspacePath`（通过 `Workspace.Settings`） |
| `hooks.New` | `cfg.Workspace.Root` | `workspacePath`（通过 `Workspace.Hooks`） |
| `approve.NewRuleStore` | `cfg.Workspace.Root` | `workspacePath`（通过 `Workspace.RuleStore`） |
| `skills` | 已在 `WorkspaceRegistry` 中按工作空间管理 | 无需改动 |

**实施方式**：与 MCP 一致，将这些资源纳入 `Workspace` 结构体，由 `WorkspaceRegistry.Get()` 懒加载。

### Phase 4：删除 `config.yaml.workspace`（随 Phase 1 或紧跟其后）

**目标**：按第 5 节清单一次性删除 `WorkspaceConfig` 及全部引用。不是"后续版本"的废弃流程 — 项目未上线，直接删。可与 Phase 3 合并为一个 PR（删除字段会强制 Phase 3 的显式传参改造）。

---

## 7. 设置/权限/钩子工作空间化

MCP 隔离方案同样适用于 `settings`、`permissions`、`hooks`，它们当前都有同样的模式：在 `BuildRegistry` 或启动时使用 `cfg.Workspace.Root` 一次性加载，而非按 `workspacePath` 加载。

### 7.1 当前问题点

```go
// runner.go 或 serve_builder.go 中类似模式：
settings.Load(cfg.Workspace.Root)          // 项目设置
hooks.New(cfg.Workspace.Root)              // 钩子工作目录
settings.ResolveVerifyFrom(cfg.Workspace.Root) // verify 命令
approve.NewRuleStore(cfg.Workspace.Root)   // 权限持久化
```

### 7.2 统一改造路径

将以上组件纳入 `Workspace` 结构体：

```go
type Workspace struct {
    Path      string
    Store     store.Store
    Skills    *skills.Registry
    Settings  *settings.Settings     // 新增
    Hooks     *hooks.Registry       // 新增
    RuleStore *approve.RuleStore    // 新增

    MCPManager *mcp.Manager
    ToolReg    *tools.Registry
}
```

`ServeRunBuilder.Build()` 中统一使用：

```go
ws := b.WSReg.Get(workspacePath)
runner := BuildRunner(
    b.Cfg, mc, provider,
    ws.ToolReg,
    ws.Skills,
    ws.Settings,    // ← 按工作空间
    ws.Hooks,       // ← 按工作空间
    ws.RuleStore,   // ← 按工作空间
)
```

### 7.3 实施优先级

1. **Phase 1**：MCP（核心需求，直接解决问题文档所述 bug）
2. **Phase 3**：Settings + Hooks + Permission（架构一致性，可独立推进）
3. 技能：已在 `WorkspaceRegistry` 中管理，无需改动，但其 `Get(workspacePath)` 参数与 `cfg.Workspace.Root` 混用问题待 Phase 3 统一

---

## 8. 风险与注意事项

### 8.1 资源开销

每个工作空间启动独立的 MCP 子进程，在极端场景（100+ 个工作空间）下可能导致文件描述符和内存压力。

**缓解措施**：
- `Prune` 驱逐闲置工作空间，释放资源
- 最大工作空间缓存数限制（如 50 个）
- 共享无状态 MCP 服务器（如纯 HTTP 的只读服务器）：Phase 2 可引入 `sharedServer` 标记

### 8.2 兼容性说明

- `config.yaml.workspace` 直接删除（见第 5 节）— 项目未上线，无旧部署需要兼容
- 启动时不加载 MCP 不影响已有功能（工具从工作空间级懒加载）
- 客户端必须发送 `workspace_path`（AgentKit 已满足；CLI serve 默认传 CWD）

### 8.3 测试覆盖

- 单工作空间基本功能
- 多工作空间并发隔离
- 工作空间无 `.mcp.json`
- `.mcp.json` 热重载
- MCP 服务器崩溃恢复
- 工作空间驱逐与资源清理
- 客户端工具与工作空间 MCP 工具合并
- 向后兼容（`workspacePath` 为空）

### 8.4 现有注释更新

`internal/runtime/workspace.go` 第 15-16 行的注释：

```go
// 旧：
// Tools are NOT workspace-scoped (they receive their workspace via
// ExecutionContext at call time).

// 新：
// Tools are workspace-scoped. Each Workspace instance manages its own
// MCP connections and tool registry loaded from <workspace>/.mcp.json.
// Built-in (non-MCP) tools remain shared and receive their workspace
// via ExecutionContext at call time.
```

---

## 9. 总结

本方案以现有 `WorkspaceRegistry` 为基石，将 MCP Manager 与工具注册表按工作空间独立管理，实现与 Claude Code 一致的工作空间级 MCP 隔离。方案选择扩展而非新建并行系统，最小化架构变更面。设置、权限、钩子等具有相同模式的组件可沿相同路径逐步统一，最终实现完整的工作空间隔离。
