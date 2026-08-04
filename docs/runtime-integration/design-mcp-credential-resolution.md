# MCP 服务器凭证的两级查询（credential.Resolver 接入）

> **状态**: 设计探索阶段（design-connection-flattening §6 / §11 与 F5.2 的落地设计）
> **受众**: Runtime 开发 + AgentKit 开发
> **版本**: v0.1
> **日期**: 2026-08-04

---

## 1. 背景与问题

设计文档 §6 定案「MCP 服务器走同一两级凭证查询」（§11：MCP 与 Connection 只共享凭证解析，生命周期/工具图独立）。当前实现**未落地**——`internal/mcp` 没有任何 `credential.Resolver` 引用，MCP server 凭证只走 `.mcp.json` 的 `${VAR}` 环境变量展开（`internal/mcp/config.go:310-328`）。

### 1.1 现状断点

```
.mcp.json:  headers: {"Authorization": "Bearer ${GITHUB_TOKEN}"}
              │
              ▼
        normalize → expandServer → os.LookupEnv("GITHUB_TOKEN")   ← 解析时一次性，静态
              │
              ▼
        ServerConfig.Headers = {"Authorization": "Bearer <env值>"} ← 字符串已固化
              │
        manager → httpClientWithHeaders(s.Headers)  ← 用固化字符串建 client（manager.go:131-156）
```

三个断点：

1. **只能读进程环境变量**——`secretsJSON` 注入的凭证（`StaticResolver`）到不了这里，因为 `expandServer` 直接 `os.LookupEnv`，不经 resolver。
2. **解析时快照**——token 过期后无法刷新（`auth_expired` 场景失效）；`ServerConfig.Headers` 在 `normalize` 时就固化，之后改 token 不生效。
3. **iOS 沙盒无环境变量**——嵌入式完全无法给 MCP server 注入凭证，`RemoteServers`（http/sse）本应在 iOS 可用，但凭证断点使需要鉴权的 MCP server（如 GitHub）在 iOS 上无法连接。

### 1.2 目标

让 MCP server 凭证与 model connection 凭证**统一走两级 resolver**：

```
Resolver(目标 = MCP server):
  1. 会话覆盖层   ← per-turn WS credential（同 model connection）
  2. server 声明的单一来源 ← injected（secretsJSON）/ env:${VAR} / none
```

保持 `.mcp.json` 的 Claude 兼容性（`${VAR}` env 展开仍可用），**新增**显式 credential 声明作为可选路径。

---

## 2. 设计方案

### 2.1 `ServerConfig` 新增字段（`internal/mcp/config.go`）

```go
type ServerConfig struct {
    Name    string            `json:"-"`       // map key in `mcpServers`
    Type    string            `json:"type"`    // stdio | http | sse
    Command string            `json:"command"` // stdio
    Args    []string          `json:"args"`
    Env     map[string]string `json:"env"`
    URL     string            `json:"url"`
    Headers map[string]string `json:"headers"` // http | sse: 静态 header（env 展开）

    // Credential 引用 credentials section 的条目（如 {llm, github}）。
    // 设置后，该 server 的凭证经 credential.Resolver 两级查询活值注入
    // HeaderName 指定的 header。与 Headers 并存：静态 header 仍生效，
    // 命中的 header 由解析值覆盖。向后兼容——旧配置无此字段仍走 env 展开。
    Credential credential.Target `json:"credential,omitempty"`
    // HeaderName 是活值注入的 header 名（默认 "Authorization"，值加 "Bearer " 前缀）。
    // 空串时用默认值。
    HeaderName string `json:"header_name,omitempty"`
}
```

**类型选择（关键约束）**：用 `credential.Target` 而非 `app.CredentialRef`——`internal/app` 已 import `internal/mcp`（`config.go:13` 的 `MCP mcp.Config` 字段），若 mcp import app 会循环依赖。`credential.Target` 与 `CredentialRef` 结构相同（Namespace/Name），有 `String()`/`Target()` 转换，无依赖。

### 2.1b 三源 header 值声明（对齐 pi）

调研 pi（`packages/coding-agent/src/core/resolve-config-value.ts`）后补充：pi 对所有配置值（API key / header）用**统一三源解析**——`!command`（shell 命令取 stdout，带缓存）、`$ENV_VAR`/`${ENV_VAR}`（环境插值）、字面量；并配套逐 header 解析（`resolveHeaders`）、缺失预检（`isConfigValueConfigured`，声明存在性不触网）、明确错误（`resolveConfigValueOrThrow`）。

code-agent 的 `.mcp.json` header 值声明对齐此模型，但扩展一个**第四来源**（resolver 活值）：

| 来源 | 语法 | 解析 | 刷新 |
|---|---|---|---|
| 命令 | `!command` | 执行命令取 stdout（可包缓存，pi 先例 `commandResultCache`） | 缓存 TTL |
| 环境变量 | `$VAR` / `${VAR}` | env 插值（现有 `expandServer` 已支持） | 无（进程 env 静态） |
| 字面量 | `"sk-..."` | 原样 | 无 |
| **resolver 活值**（新增） | `credential:<namespace>/<name>` 或 `ServerConfig.Credential` 字段 | `credential.Resolver` 两级查询 | **每次请求**（JWT 过期可刷新） |

`ServerConfig.Headers` 的每个值独立声明来源：

```json
{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "Authorization": "Bearer ${GITHUB_TOKEN}"        // env 插值（Claude/pi 兼容）
      }
    },
    "secure-svc": {
      "type": "http",
      "url": "https://internal.example.com/mcp/",
      "headers": {
        "Authorization": "!op read op://vault/mcp/token" // 命令（1Password，pi 风格）
      }
    },
    "gitlab": {
      "type": "http",
      "url": "https://gitlab.example.com/mcp/",
      "credential": {"namespace": "llm", "name": "gitlab"},  // resolver 活值（code-agent 增强）
      "header_name": "Authorization"
    }
  }
}
```

**实现要点**：
- `expandServer` 已有 env 插值（`${VAR}`）——`!command` 前缀检测是新增的、轻量的（在 `expand` 里识别 `!` 起始，执行命令）
- `credential:` 字段走 resolver（§2.2 的 round tripper 活值路径），不经过 `expandServer`
- 三者共存：静态 env/命令 header + resolver 活值 header 可同时存在，解析优先级 resolver > env/命令 > 字面量

**为什么值得**：GitHub token 用 `$GITHUB_TOKEN`（简单）或 `credential: {llm, github}`（secretsJSON 注入，iOS 可用）；内部服务用 `!op read`（安全壳）。同一 `.mcp.json` 里按 server 选择最合适的来源——这是 pi 的灵活性 + code-agent 的活值/注入能力的结合。

### 2.2 `headerRoundTripper` 改造（`internal/mcp/manager.go`）

现状是固定 headers map（:171-182）。增加活值解析：

```go
type headerRoundTripper struct {
    base     http.RoundTripper
    headers  map[string]string    // 静态（env 展开的）
    resolver credential.Resolver  // 非 nil 时每次请求解析活值
    target   credential.Target    // Credential 引用的 target
    header   string               // 注入到哪个 header（默认 "Authorization"）
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
    req = req.Clone(req.Context())
    for k, v := range h.headers {
        req.Header.Set(k, v)
    }
    if h.resolver != nil {
        if c, err := h.resolver.Resolve(req.Context(), h.target); err == nil && !c.IsZero() {
            req.Header.Set(h.header, "Bearer "+c.Secret)
        }
    }
    return h.base.RoundTrip(req)
}
```

每次请求解析（活值语义），与 `OpenAICompatibleProvider.applyAuth` 同构。`credential.CachedResolver` 可包装以控制解析频率（TTL 内缓存，到期前刷新）。

### 2.3 `newTransport` 装配（`internal/mcp/manager.go:131-156`）

`newTransport` 增加 resolver 参数：

```go
func newTransport(s ServerConfig, resolver credential.Resolver) (mcpsdk.Transport, *exec.Cmd, error) {
    switch s.Type {
    case TransportHTTP:
        return &mcpsdk.StreamableClientTransport{
            Endpoint:   s.URL,
            HTTPClient: httpClientWithHeaders(s.Headers, resolver, s.Credential, headerNameFor(s)),
        }, nil, nil
    // ... SSE 同构；stdio 的 Env 也可经 resolver 展开（见 §2.5）
    }
}
```

`httpClientWithHeaders` 增加参数：静态 headers + 可选 resolver/target/header 名。`s.Credential.IsZero()` 时行为与现状完全一致（纯静态）。

### 2.4 `Manager` 持有 resolver（装配点）

`mcp.NewManager(trace io.Writer)`（:49）改为 `NewManager(trace io.Writer, resolver credential.Resolver)`。装配点 3 处：

| 装配点 | 传什么 resolver |
|---|---|
| `internal/runtime/registry.go:261` | `cfg.CredentialResolver(nil)`（含 injected secrets + env） |
| `internal/runtime/workspace.go:191` | 同上（workspace 级 MCP） |
| `internal/runtime/workspace.go:349` | 同上（reload 路径） |

**会话覆盖（两级的第一级）**：`ServeRunBuilder` 已有 `effectiveCredentialResolver(session, base)`（会话 → 基链）。MCP 装配若也用同一 base resolver，per-turn 会话凭证即可覆盖 MCP server——这是「两级」的完整落点。但 MCP session 是跨 turn 长活（`Manager.sessions` 缓存），会话级凭证按 turn 变化时，client 是复用的——**需评估**：会话覆盖对 MCP 是否按连接建立时解析，还是每次请求解析（round tripper 每次请求调 resolver，天然支持）。

### 2.5 stdio server 的 env 展开（可选增强）

stdio server 的凭证通过 `Env` 传入子进程（`cmd.Env`，manager.go:148-151）。若 server 声明了 Credential，`Env` 中引用的变量值也可经 resolver 解析（而非仅 `os.LookupEnv`）。**此为非必选**——stdio 在 iOS 沙盒禁用，桌面场景环境变量通常可用；先支持 http/sse 的 header 注入，stdio env 解析作为后续。

---

## 3. 与现有机制的关系

### 3.1 `.mcp.json` 语法（向后兼容）

```
现状（Claude 兼容，不变）：headers: {"Authorization": "Bearer ${GITHUB_TOKEN}"}   ← env 展开
新增（可选）：            credential: {namespace: "llm", name: "github"}
                          header_name: "Authorization"                          ← 两级 resolver
```

- 旧配置无 `credential` 字段 → 行为完全不变（env 展开路径）
- 新配置声明 `credential` → 经 resolver 活值注入，覆盖同名静态 header

### 3.2 与 `injectSecrets` 的对接

`injectSecrets` 已按 `credential.Target` 索引注入（`llm/github` 或 `mcp/github`）。MCP server 声明 `credential: {llm, github}` 后，secretsJSON 注入的 token 即被 resolver 命中——**嵌入式场景打通**（iOS Keychain → secretsJSON → resolver → MCP header）。

### 3.3 与 secretsJSON 三形态

`CredentialTarget.id`（`{namespace}/{name}`）与 flat id 都能经 `TargetFromConnectionID` 映射到 `credential.Target`。MCP 用 namespaced target（`mcp/github` 或 `llm/github`），不参与 flat connection id 扁平化（§11：MCP 独立）。

---

## 4. 实现步骤

1. **`ServerConfig` 加字段**（config.go）——`Credential credential.Target` + `HeaderName string`，JSON tag 向后兼容
2. **`headerRoundTripper` 改造**（manager.go）——加 resolver 活值解析，每次请求
3. **`newTransport` 加 resolver 参数**（manager.go:131）——`httpClientWithHeaders` 扩展
4. **`NewManager` 加 resolver 参数**（manager.go:49）——装配点 3 处更新
5. **测试**：
   - 单测：`headerRoundTripper` 静态 + 活值混合、resolver 返回空时回退静态、`Credential.IsZero()` 时纯静态
   - 集成：伪造 resolver（StaticResolver）+ httptest server，验证 Authorization header 正确注入
6. **GitHub MCP 真实测试**（见 §5）

---

## 5. GitHub MCP 验证计划（真实场景）

### 5.1 配置

`~/.codeagent/mcp.json`：

```json
{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "credential": {"namespace": "llm", "name": "github"},
      "header_name": "Authorization"
    }
  }
}
```

### 5.2 凭证注入（三选一）

- **env**：`export GITHUB_TOKEN=ghp_xxx` + credentials section `llm.github: {source: env, env: GITHUB_TOKEN}` —— env 链
- **secretsJSON 注入**（嵌入式）: `{"llm%2Fgithub": {"type": "bearer", "secret": "ghp_xxx"}}` —— injected 链
- **会话覆盖**（测试两级）: per-turn WS 凭证带 `llm/github`

### 5.3 验证清单

- [ ] TUI 启动 → `mcp.github` 工具注册成功（discover tools = 凭证有效）
- [ ] 调一个 GitHub 工具（如列出仓库/读 issue）→ 返回真实数据
- [ ] token 过期 → 换 token → 不重启 client 仍工作（活值语义验证）
- [ ] `Credential.IsZero()` 的旧配置（纯 env 展开）行为不变
- [ ] stdio server 无回归（command 路径不受 header 改造影响）
- [ ] iOS 嵌入式：RemoteServers + secretsJSON 注入 → GitHub MCP 可用

---

## 6. 风险与决策点

| 项 | 风险 | 决策 |
|---|---|---|
| mcp ↔ app 循环依赖 | 高 | 用 `credential.Target` 而非 `app.CredentialRef`（§2.1，已确认 app import mcp） |
| 会话覆盖对长活 MCP client | 中 | round tripper 每次请求调 resolver，天然支持按请求解析；确认 per-turn 覆盖是否必要（或仅连接时解析） |
| 解析失败 fallback | 中 | resolver 返回 error/zero 时：跳过注入（保留静态 header）还是 fail client？倾向前者（与 model applyAuth 的宽松一致） |
| stdio env 解析 | 低 | 先不做（iOS 禁 stdio，桌面 env 可用），作为后续 |
| 请求级解析性能 | 低 | 可包 `CachedResolver` 控制频率（TTL 内缓存） |

---

## 7. 验收标准

- MCP server 凭证与 model 凭证统一走 `credential.Resolver`（两级：会话覆盖 + 单一来源）
- secretsJSON 注入的凭证可到达 MCP server（嵌入式场景打通）
- 活值语义：token 刷新后 MCP client 不重启即生效
- 向后兼容：无 `credential` 声明的旧 `.mcp.json` 行为完全不变
- GitHub MCP 真实场景通过 §5.3 全部验证
