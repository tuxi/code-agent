# code-agent Workspace / MCP 配置边界问题发现

状态：待 code-agent 侧架构探索

## 问题摘要

当前 `codeagentd` 在进程启动时从 `config.yaml` 读取：

```yaml
workspace:
  root: .
```

随后使用这个 root 解析 `.mcp.json` 并创建全局 MCP Manager。由于 `codeagentd` 通常从
`code-agent` 仓库目录启动，实际读取的是：

```text
code-agent/.mcp.json
```

而不是 AgentKit 创建对话时指定的 workspace，例如：

```text
AgentKit/.mcp.json
```

AgentKit 创建 conversation 时已经会传递 `workspace_path`，但这个 workspace 只进入
session、skills、assets 和文件工具路径，没有参与 MCP 配置解析。因此当前 MCP 配置
绑定的是 daemon 启动目录，而不是实际 conversation workspace。

## 现状证据

### 1. MCP 在 daemon 启动阶段解析

当前代码路径：

```text
cmd/codeagentd/main.go
  app.LoadConfig("config.yaml")
  mcp.ResolveDesktop(cfg.Workspace.Root, ...)
  runtime.BuildRegistry(...)
```

相关位置：

- `cmd/codeagentd/main.go`：加载配置并解析 MCP；
- `internal/runtime/registry.go`：连接 MCP server 并把 tools 注册到全局 registry；
- `internal/runtime/serve_builder.go`：每轮只根据 session workspace 设置 runner root，复用全局
  `ToolReg`。

### 2. Conversation workspace 是后置决定的

```text
CodeAgentMac 选择 workspace
  ↓
POST /v1/conversations { workspace_path: ... }
  ↓
session.WorkspacePath
```

此时 MCP Manager 已经创建完成，后续不会自动读取该 workspace 的 `.mcp.json`。

### 3. 当前行为可复现

当 `codeagentd` 在 `code-agent` 目录启动、AgentKit workspace 为 `AgentKit` 时：

```text
实际 MCP 配置：code-agent/.mcp.json
期望 MCP 配置：AgentKit/.mcp.json
```

把 `.mcp.json` 放入 AgentKit 对当前 daemon 不生效，除非修改 daemon 默认 root 或改变启动环境。

## 为什么这是架构问题

### 1. workspace 语义不一致

当前系统同时存在两个 workspace：

```text
daemon workspace
  = config.yaml.workspace.root

conversation workspace
  = AgentKit 创建 conversation 时传入的 workspace_path
```

但 MCP、权限规则、工具目录和部分运行时状态仍绑定 daemon workspace，导致 AgentKit UI
展示的工作区和 Agent Runtime 实际使用的 MCP 工具集可能不是同一个项目。

### 2. 多 workspace 会互相污染

如果同一个 `codeagentd` 同时服务多个 workspace：

```text
Workspace A → .mcp.json A
Workspace B → .mcp.json B
```

当前全局 MCP Manager 只能提供一套工具，可能出现：

- A 的工具出现在 B 的对话中；
- B 的 server credentials 被 A 使用；
- server-scoped approval 跨 workspace 生效；
- workspace-specific MCP server 生命周期无法关闭；
- 同名 MCP server 的配置相互覆盖。

### 3. `config.yaml.workspace` 责任过重

当前 `workspace.root` 同时影响：

- MCP 配置解析；
- 默认 workspace；
- telemetry / SQLite store；
- skills 加载；
- permission rule store；
- 部分内置工具默认路径。

因此不能未经盘点直接删除字段。需要先拆分“daemon 默认目录”和“conversation 实际工作区”
两个概念。

## 需要 code-agent 侧探索的问题

### A. workspace 的权威来源

请明确以下优先级：

```text
conversation.workspace_path
  > daemon default workspace
  > process current directory
```

`config.yaml.workspace` 是否应该：

1. 完全移除；
2. 改名为 `default_workspace`，只作为 CLI/TUI 无 conversation 场景的 fallback；
3. 保留用于 daemon 数据目录，但不再参与 MCP 配置解析。

建议不要让 `.` 同时代表 daemon 目录和用户项目目录。

### B. MCP Manager 生命周期

需要比较以下方案：

#### 方案 1：workspace 级 MCP Manager

```text
WorkspaceRegistry
  ├── WorkspaceInstance A
  │     ├── tools registry
  │     └── MCP Manager A
  └── WorkspaceInstance B
        ├── tools registry
        └── MCP Manager B
```

优点：配置、工具、credentials 和 server 生命周期天然隔离。

缺点：需要 workspace cache、并发关闭、server 重连和资源回收策略。

#### 方案 2：每个 conversation 独立 MCP Manager

优点：隔离最清楚。

缺点：启动成本高，同一个 workspace 的多个 conversation 会重复启动 server。

#### 方案 3：daemon 全局 MCP Manager + workspace overlay

只适合完全 user-scope 的 server，不适合作为 project `.mcp.json` 的默认模型。

### C. 工具目录如何进入每轮 Runner

当前 `BuildRunner` 每轮读取 `session.WorkspacePath`，但复用全局 `ToolReg`。需要明确：

- workspace-specific built-in tools 如何创建；
- workspace-specific MCP tools 如何合并；
- tool namespace 冲突如何处理；
- MCP tools 是否在 turn 边界冻结；
- `.mcp.json` 修改后何时生效；
- server 重启期间工具如何标记 unavailable。

### D. 权限与凭据隔离

MCP 配置支持 `env`、HTTP headers 和 user/project/local scope。需要保证：

- credentials 不跨 workspace 泄露；
- project `.mcp.json` 不回显 secret；
- permission allowlist 绑定 workspace + server；
- workspace A 的 `mcp__github__*` 规则不能自动授权 workspace B；
- server 关闭后其 pending calls 和 approvals 正确终止。

## 推荐目标架构

```text
CodeAgentMac
  ↓ create conversation(workspace_path)
codeagentd
  ↓ resolve workspace instance
WorkspaceRegistry
  ↓
workspace/.mcp.json
  ↓
workspace-scoped MCP Manager
  ↓ tools/list
workspace-scoped Tool Registry
  ↓
per-turn Runner
```

其中：

- `config.yaml` 不再决定 project MCP 配置；
- `.mcp.json` 是 workspace MCP 的唯一来源；
- `conversation.workspace_path` 是 workspace 的权威标识；
- `config.yaml` 最好删除，因为歧义太大，如保留 workspace，只能作为默认 fallback 或 daemon 数据目录配置；
- AgentKit 不需要自行解析 `.mcp.json`；
- AgentKit 只通过 Agent Wire 接收该 workspace 对应的工具和结果。

## 最小验证用例

### 用例 1：daemon 启动目录与 workspace 不同

```text
启动目录：code-agent/
conversation workspace：AgentKit/
```

预期：加载 `AgentKit/.mcp.json`，不加载 `code-agent/.mcp.json`。

### 用例 2：两个 workspace 使用不同 server

```text
Workspace A/.mcp.json → server_a
Workspace B/.mcp.json → server_b
```

预期：

- A 只能看到 `mcp__server_a__*`；
- B 只能看到 `mcp__server_b__*`；
- server 进程和 credentials 不交叉。

### 用例 3：切换 conversation

```text
打开 A → 创建 A 的 MCP Manager
切换 B → 创建/复用 B 的 MCP Manager
返回 A → 复用 A 的 MCP Manager
```

预期：工具目录与 workspace 一致，不需要重启整个 `codeagentd`。

### 用例 4：workspace `.mcp.json` 修改

预期必须定义清楚：

- 下一次 conversation 生效；或
- 显式 reload 生效；或
- 自动 reload 生效。

不能继续使用“daemon 下次重启才生效”作为隐式行为。

## 验收标准

该问题关闭前，至少需要满足：

- codeagentd 从任意目录启动都不影响 conversation workspace 的 MCP 配置；
- AgentKit/.mcp.json 能被 AgentKit workspace 对话实际加载；
- 多 workspace 工具目录、server 进程、credentials、approval 隔离；
- `.mcp.json` 的 project/local/user scope 行为有明确测试；
- MCP Manager 生命周期在 workspace 创建、切换、关闭时可观测且可回收；
- `config.yaml.workspace` 的最终职责被文档化，并不再作为 project MCP 的隐式根目录；
- AgentKit 无需硬编码任何特定 MCP server 名称或路径。

## 当前临时 workaround

为了单 workspace 验证，可以把 `config.yaml.workspace.root` 临时设为 AgentKit 的绝对路径，
然后重启 codeagentd。但这只能用于验证，不能作为最终架构；它会让所有 conversation 共用
AgentKit 的 MCP 配置。
