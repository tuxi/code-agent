# Code-Agent Runtime — Gateway Integration Contract v1

> **状态**: 设计阶段，待评审
> **角色**: 本文件是 Code-Agent Runtime 与 Agent Gateway 之间的集成契约。它不定义 Gateway API（见 `agent-gateway-api-v1.md`），不定义 Credential 抽象（见 `design-credential-gateway.md`），而是定义 Runtime 如何**消费** Gateway 提供的模型服务。
>
> **前置依赖**:
> - [Agent Gateway API v1.1](../agent-gateway-api/agent-gateway-api-v1.md) — 已冻结
> - [Credential & Gateway Design v2.1](./design-credential-gateway.md) — 本文件的抽象基础
>
> **原则**: Runtime 不实现 token 刷新、不持有 refresh_token、不调用 `/auth/refresh`。Runtime 只消费 token。

---

## 三层 Contract 关系

```
AgentKit Credential Model v1        ← 已冻结
        │
        │ "AgentKit 如何管理 token"
        │
        ▼
Agent Gateway API v1.1               ← 已冻结
        │
        │ "Gateway 提供什么端点"
        │
        ▼
Runtime Gateway Integration v1       ← 本文件
        │
        │ "Runtime 如何消费 Gateway"
        │
        ▼
Runtime Credential Abstraction v2.1  ← 已冻结
```

本文件是连接 Gateway API Contract 和 Runtime Credential Abstraction 的**集成契约**。

---

## 1. Runtime 视角的 Gateway

### 1.1 Gateway 对 Runtime 是什么

从 Runtime 的角度看，Agent Gateway 就是一个 **OpenAI-compatible endpoint**，唯一的区别是：

| 维度 | Gateway | BYOK Provider |
|------|---------|---------------|
| 端点 | `POST /api/v1/agent/chat/completions` | `POST /v1/chat/completions` |
| 认证 | `Authorization: Bearer <JWT>` | `Authorization: Bearer <API Key>` |
| 模型选择 | 由 Gateway 服务端路由 | 由 Runtime 客户端指定 |
| Usage 追踪 | Gateway 服务端 | 客户端自行估算 |
| Token 刷新 | **Runtime 不负责**，宿主管理 | 不适用（API key 不过期） |

### 1.2 Runtime 看到什么

```
Runtime 视角:

    model.Provider 接口
          │
          │ Complete(ctx, req) → Response
          │
          ▼
    OpenAICompatibleProvider
          │
          │ BaseURL:  https://agent.xxx.com/api/v1/agent
          │ CredentialResolver:  ← 宿主注入
          │
          ▼
    HTTP POST /chat/completions
    Authorization: Bearer <token>
```

Runtime 不知道这是 Gateway 还是直连。它只知道一个 endpoint + 一个 credential。

---

## 2. Runtime 配置

### 2.1 最小配置

```yaml
# config.yaml — Gateway 模式最小配置

models:
  default:
    provider: openai                          # OpenAI-compatible 协议
    base_url: "https://agent.xxx.com/api/v1/agent"
    model: ""                                 # 由 Gateway 服务端选择
    credential:
      namespace: gateway
      name: default
```

### 2.2 完整配置

```yaml
# config.yaml — 完整 Gateway + BYOK 混合配置

# ── Credential 来源 ──
credentials:
  gateway:
    source: injected                         # 宿主注入（AgentKit secretsJSON 或 CLI --gateway-token）
  deepseek:
    source: env
    env: DEEPSEEK_API_KEY
  ollama:
    source: none

# ── Models ──
models:
  # Gateway — 模型选择在服务端
  agent:
    provider: openai
    base_url: "https://agent.xxx.com/api/v1/agent"
    model: ""                                 # 空 = Gateway 默认模型
    credential:
      namespace: gateway
      name: default
    context_window: 128000
    # 注意：定价信息由 Gateway API 返回，不在 Runtime 配置中硬编码

  # 指定 Gateway 模型
  agent-pro:
    provider: openai
    base_url: "https://agent.xxx.com/api/v1/agent"
    model: "deepseek-v4-pro"                  # 透传给 Gateway
    credential:
      namespace: gateway
      name: default

  # BYOK 直连 — 与 Gateway 共存
  deepseek:
    provider: openai
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-flash"
    credential:
      namespace: llm
      name: deepseek

  # 本地模型 — 无 credential
  ollama:
    provider: ollama
    base_url: "http://localhost:11434"
    model: "qwen3-coder-tool"
```

### 2.3 配置字段说明

| 字段 | 必需 | 说明 |
|------|------|------|
| `models.<name>.provider` | 是 | `openai` — Gateway 使用 OpenAI-compatible 协议 |
| `models.<name>.base_url` | 是 | Gateway 的 chat completions 端点 |
| `models.<name>.model` | 否 | 透传给 Gateway；空字符串 = Gateway 使用默认模型 |
| `models.<name>.credential.namespace` | 是 | `gateway` |
| `models.<name>.credential.name` | 是 | credential 实例名，通常为 `default` |
| `credentials.<name>.source` | 是 | `injected`（宿主注入）、`env`（环境变量）、`none`（无认证） |

---

## 3. Runtime 如何使用 Gateway Credential

### 3.1 核心原则：Runtime 不知道 Gateway

Runtime 内部不出现 `gateway` 包、`TokenProvider` 接口、或任何 Gateway 专有概念。

Runtime 只知道：

```
我需要 credential
     │
     ▼
credential.Resolver.Resolve(ctx, target)
     │
     ▼
Credential{Type: Bearer, Secret: "..."}
     │
     ▼
Authorization: Bearer ...
```

Gateway 对 Runtime 而言只是一个 `Target{Namespace: "gateway", Name: "default"}`。这个 Target 和其他 Target（`llm/deepseek`、`mcp/github`）没有任何区别。

### 3.2 宿主注入

宿主（AgentKit/CLI）负责把 token 包装成 `credential.Resolver`，Runtime 只消费：

```
宿主层                                Runtime 层
───────                              ──────────

AgentKit:
  Keychain.get("access_token")
       │
       ▼
  credential.StaticResolver{
      {Namespace:"gateway", Name:"default"}: {
          Type:   credential.Bearer,
          Secret: token,
      },
  }
       │
       │ secretsJSON / Reconfigure
       ▼
                                 credential.ChainResolver{
                                     Resolvers: []credential.Resolver{
                                         staticResolver,    // ← 宿主注入的 token
                                         credential.EnvResolver{},
                                         credential.NewFileResolver(...),
                                     },
                                 }
                                       │
                                       ▼
                                 credential.Resolver  ← Runtime 只看到这个
```

### 3.3 为什么不在 Runtime 中定义 TokenProvider

```
如果 Runtime 定义了 TokenProvider:
  Runtime 知道 "Gateway 有 token 这个概念"
  Runtime 出现 gateway 包
  CLI 场景也需要实现 TokenProvider
  → 商业概念渗入开源 core

现在：
  Runtime 只知道 credential.Resolver
  Gateway 只是其中一个 Target.Namespace
  Token 只是 Credential{Type: Bearer, Secret: "..."}
  Runtime 完全不知道 Gateway 存在
```

### 3.4 StaticResolver 承载 Gateway token

```go
// 宿主构造 StaticResolver — 这是 credential 包的现有类型，无需新增任何代码
gwTarget := credential.Target{Namespace: "gateway", Name: "default"}
gwCred := credential.Credential{
    Type:      credential.Bearer,
    Secret:    accessToken,
    ExpiresAt: &expiry,
}
resolver := credential.StaticResolver{
    gwTarget: gwCred,
}

// 加入 Chain
chain := &credential.ChainResolver{
    Resolvers: []credential.Resolver{
        resolver,                        // 1. 宿主注入（优先级最高）
        credential.EnvResolver{},        // 2. 环境变量 fallback
        credential.NewFileResolver(home),// 3. 磁盘文件 fallback
    },
}

// Runtime 使用
provider := model.NewOpenAICompatibleProvider(
    "https://agent.xxx.com/api/v1/agent",
    chain,
    gwTarget,
)
```

---

## 4. Token 生命周期（Runtime 视角）

### 4.1 正常流程

```
请求 → credential.Resolver.Resolve(ctx, target) → Bearer token → Authorization header → 发送请求 → 200 OK
```

### 4.2 Token 过期流程

```
请求 → Resolver.Resolve(ctx, target) → Bearer token → Authorization header → 发送请求
                                                                          │
                                                                    401 Unauthorized
                                                                          │
                                                                          ▼
                                                            OpenAICompatibleProvider
                                                            (或 ResilientProvider)
                                                                          │
                                                                          ▼
                                                                返回 APIError{StatusCode: 401}
                                                                          │
                                                                          ▼
                                                            Agent Loop 收到 error
                                                                          │
                                                                          ▼
                                                            判断 isRetryable(401) → false
                                                                          │
                                                                          ▼
                                                            停止重试，向上传播 error
                                                                          │
                                                                          ▼
                                                            TurnExecutor
                                                                          │
                                                                          ▼
                                                            emit turn_failed / auth_expired
                                                                          │
                                                            ┌─────────────┴─────────────┐
                                                            │                           │
                                                       AgentKit                      CLI
                                                            │                           │
                                                    POST /auth/refresh          提示用户重新登录
                                                    获取新 token                codeagent login
                                                    调用 Reconfigure(token)
```

### 4.3 Runtime 的 401 处理

Runtime 对 401 的行为由 `ResilientProvider.isRetryable()` 控制：

```go
// 当前代码已经是正确行为，无需修改：
func isRetryable(err error) bool {
    var apiErr *APIError
    if errors.As(err, &apiErr) {
        switch apiErr.StatusCode {
        case 408, 429, 500, 502, 503, 504:
            return true   // 可重试的瞬时故障
        default:
            return false  // 401 在这里 → 不可重试，向上传播
        }
    }
    // ...
}
```

**401 不是可重试错误**。`ResilientProvider` 收到 401 后立即返回 error，不会重试。Agent Loop 收到 error 后结束当前 turn。

### 4.4 auth_expired 事件

当 TurnExecutor 因为 401 导致 turn 失败时，事件的 error 字段包含 401 信息。宿主层通过以下方式感知：

**AgentKit（WebSocket 事件流）**：

```json
{
  "type": "turn_failed",
  "session_id": "sess_abc",
  "error": {
    "code": "auth_expired",
    "message": "Gateway returned 401 — access token may be expired"
  }
}
```

**CLI（stderr + 退出码）**：

```
Error: Gateway authentication failed (HTTP 401).
Your access token may have expired. Run:
  codeagent login
```

**Server 模式（HTTP Response）**：

```json
{
  "error": {
    "code": "auth_expired",
    "status": 401
  }
}
```

### 4.5 Reconfigure — token 热切换

宿主刷新 token 后，调用 Runtime 的 `Reconfigure` 注入新 token：

```go
// 当前已存在的方法，无需新增 API
func (s *Server) Reconfigure(secretsJSON, modelName string) error
```

AgentKit 侧调用：

```swift
// AgentKit — Swift
let newToken = try await authClient.refresh()
let secrets = """
{
  "gateway/default": {
    "type": "bearer",
    "secret": "\(newToken)",
    "expires_at": \(newTokenExpiry)
  }
}
"""
try server.reconfigure(secrets, modelName: "")
```

CLI 侧 — `FileResolver` 自动读取更新的 `~/.codeagent/credentials` 文件。

---

## 5. Gateway Provider 构造

### 5.1 不是新类型

`GatewayProvider` 不存在。它只是 `OpenAICompatibleProvider` + gateway credential target。

```go
// 创建 Gateway provider — Runtime 内部无 TokenProvider，直接使用 credential.Resolver
func buildGatewayProvider(endpoint string, resolver credential.Resolver) model.Provider {
    target := credential.Target{Namespace: "gateway", Name: "default"}
    return model.NewOpenAICompatibleProvider(endpoint, resolver, target)
}
```

`resolver` 是宿主（AgentKit/CLI）构造的 `ChainResolver` 或 `StaticResolver`。Runtime 不知道它背后是 Keychain 还是环境变量。

Gateway 和 BYOK 的唯一区别是 credential target：

| Provider | BaseURL | Credential.Namespace | Resolver 来源 |
|----------|---------|---------------------|-------------|
| Gateway | `https://agent.xxx.com/api/v1/agent` | `gateway` | `StaticResolver`（宿主注入 token） |
| DeepSeek BYOK | `https://api.deepseek.com` | `llm` | `EnvResolver`（环境变量） |
| Ollama | `http://localhost:11434` | 无 | 无 |

三者都使用同一个 `OpenAICompatibleProvider` 实现。

### 5.2 构造流程

```
                   BuildProvider(mc, pc, credResolver)
                            │
                            │ mc.Provider == "openai"
                            │ mc.Credential.Namespace == "gateway"
                            │
                            ▼
                   credResolver 是 ChainResolver/StaticResolver
                   （由宿主启动时构造，已包含 gateway token）
                            │
                            ▼
                   model.NewOpenAICompatibleProvider(
                       mc.BaseURL,   // https://agent.xxx.com/api/v1/agent
                       credResolver, // ← 宿主注入的 credential.Resolver
                       target,       // {Namespace:"gateway", Name:"default"}
                   )
                            │
                            ▼
                   ResilientProvider(inner)
                            │
                            ▼
                   Agent Loop 使用
```

### 5.3 请求流程

```
Agent Loop
    │
    ▼
ResilientProvider.Complete(ctx, req)
    │
    ▼
OpenAICompatibleProvider.Complete(ctx, req)
    │
    ├── c, err := p.Credential.Resolve(ctx, p.CredentialTarget)
    │       │
    │       ▼
    │   StaticResolver.Resolve(ctx, {Namespace:"gateway", Name:"default"})
    │       │
    │       ▼
    │   返回 Credential{Type: Bearer, Secret: "eyJhbGciOi..."}
    │
    ├── req.Header.Set("Authorization", "Bearer eyJhbGciOi...")
    │
    ▼
HTTP POST https://agent.xxx.com/api/v1/agent/chat/completions
    │
    ▼
Response / Error
```

---

## 6. 宿主注入契约

### 6.1 AgentKit 注入路径

```
AgentKit (Swift)                              Runtime (Go)
─────────────────                            ──────────────

应用启动:
  Keychain.get("access_token")
  Keychain.get("refresh_token")
       │
       ▼
  mobile.Start(
      workspaceDir,
      configYAML,        ← 包含 gateway credential 配置
      secretsJSON: {
          "gateway/default": {
              "type": "bearer",
              "secret": "<access_token>",
              "expires_at": 1234567890
          }
      }
  )
       │
       │ gomobile
       ▼
  embed.StartServer()
       │
       ▼
  injectSecrets(cfg, secrets)
       │
       ▼
  创建 StaticResolver 包含 gateway token
       │
       ▼
  BuildProvider(mc, pc, chainResolver)
       │
       ▼
  OpenAICompatibleProvider + gateway credential

─────────────────────────────────────────────

Token 过期:
  TurnExecutor 返回 401
       │
       ▼
  WebSocket event → AgentKit
       │
       ▼
  AgentKit:
      POST /api/v1/auth/refresh
      获取新 access_token
      Keychain.set("access_token", newToken)
       │
       ▼
  server.reconfigure(
      secretsJSON: {
          "gateway/default": {
              "type": "bearer",
              "secret": "<new_token>",
              "expires_at": <new_expiry>
          }
      }
  )
       │
       │ gomobile
       ▼
  Runtime.injectSecrets(cfg, newSecrets)
  Runtime.Builder.Reconfigure(mc, newProvider)
       │
       ▼
  下次 turn 使用新 token
```

### 6.2 secretsJSON 格式约定

```json
{
  "<namespace>/<name>": {
    "type": "bearer | secret | none",
    "secret": "<credential_value>",
    "expires_at": "<unix_timestamp>",
    "metadata": {
      "refresh_token": "<optional>",
      "scope": "<optional>"
    }
  }
}
```

| 字段 | 必需 | 说明 |
|------|------|------|
| `type` | 是 | `bearer`（Gateway JWT、BYOK API key、MCP OAuth token 都使用此类型）|
| `secret` | 是 | credential 值，不可展示 |
| `expires_at` | 否 | Unix 时间戳；nil = 永不过期 |
| `metadata` | 否 | 附加信息。Runtime 不解析 `refresh_token` |

### 6.3 CLI 注入路径

```
CLI 启动:
  export DEEPSEEK_API_KEY=sk-xxx                          ← BYOK
  codeagent serve --gateway-token "$(cat ~/.codeagent/gateway-token)"  ← Gateway

CLI 内部:
  chain := &credential.ChainResolver{
      Resolvers: []credential.Resolver{
          // 1. CLI flag 注入（最高优先级）
          flagResolver,                                  ← --gateway-token

          // 2. 环境变量
          credential.EnvResolver{},

          // 3. 配置文件
          credential.NewFileResolver("~/.codeagent/credentials"),
      },
  }
  BuildProvider(mc, pc, chain)

CLI 登录后:
  codeagent login                                        ← OAuth，保存到文件
  → ~/.codeagent/credentials 更新
  → FileResolver 下次请求自动读取新 token
```

---

## 7. 错误处理契约

### 7.1 Runtime 视角的错误分类

| HTTP Status | Runtime 行为 | 宿主行为 |
|-------------|-------------|---------|
| 200 | 正常 | — |
| 401 | 返回 error，不重试 | 刷新 token，调用 `Reconfigure` |
| 429 | `ResilientProvider` 重试（退避） | — |
| 5xx | `ResilientProvider` 重试（退避） | — |
| 4xx（非 401/429） | 返回 error | 检查请求参数 |

### 7.2 Gateway 特有的 4xx

Gateway 可能返回标准 OpenAI API 之外的业务错误：

| HTTP Status | Gateway Code | 含义 | Runtime 行为 |
|-------------|-------------|------|-------------|
| 402 | `quota_exceeded` | 配额不足 | 返回 error，不重试 |
| 403 | `subscription_required` | 需要订阅 | 返回 error，不重试 |
| 429 | `rate_limited` | 频率限制 | `ResilientProvider` 重试 |

Runtime 将这些错误码透传给上层（Agent Loop → TurnExecutor → 宿主事件流），不做业务语义解释。

```go
// Gateway 返回的错误对 Runtime 是透明的 APIError。
// Runtime 不解析 Gateway 特有的错误码语义。
type APIError struct {
    StatusCode int
    Body       string
    Type       string // 可选：Gateway 返回的错误类型
    Message    string // 可选：Gateway 返回的错误描述
}
```

### 7.3 错误传播路径

```
Gateway HTTP Response
        │
        ▼
OpenAICompatibleProvider
   → 解析 HTTP status
   → 构造 APIError{StatusCode, Body, ...}
        │
        ▼
ResilientProvider
   → isRetryable(APIError{StatusCode: 401}) → false
   → 不重试，立即返回 error
        │
        ▼
Agent Loop
   → 收到 error
   → 停止当前 turn
        │
        ▼
TurnExecutor
   → emit turn_failed event
   → 宿主收到事件
        │
   ┌────┴────┐
   │         │
AgentKit    CLI
刷新 token  提示登录
```

---

## 8. 与其他组件的关系

### 8.1 文档引用图

```
agent-gateway-api-v1.md          ← Gateway 服务端契约
        │
        │ "Gateway 暴露什么"
        │
        ▼
runtime-gateway-integration-v1.md ← 本文件
        │
        │ "Runtime 如何消费 Gateway"
        │
        ▼
design-credential-gateway.md     ← Credential 抽象设计
        │
        │ "credential.Resolver / Target / Credential"
        │
        ▼
code-agent/internal/             ← 实现
  credential/
  model/openai_compatible.go
  runtime/provider.go
```

### 8.2 类型依赖

```
credential.Resolver               ← design-credential-gateway.md 定义
        │                            （由宿主构造，StaticResolver 承载 token）
        │
        │ 被使用于
        ▼
model.OpenAICompatibleProvider    ← 现有类型，扩展 Credential 字段
        │
        │ 被包装于
        ▼
model.ResilientProvider           ← 现有类型，不变
        │
        │ 被使用于
        ▼
agent.Runner                      ← 现有类型，不变
```

### 8.3 责任边界（最终版）

```
┌──────────────────────────────────────────────────────────────┐
│  Agent Gateway Service                                       │
│                                                              │
│  端点: POST /api/v1/agent/chat/completions                    │
│  认证: Authorization: Bearer <JWT>                           │
│  职责: 模型路由、用量追踪、计费                                 │
│                                                              │
│  定义于: agent-gateway-api-v1.md                             │
└───────────────────────┬──────────────────────────────────────┘
                        │ HTTPS
┌───────────────────────▼──────────────────────────────────────┐
│  AgentKit / CLI（宿主层）                                     │
│                                                              │
│  实现: credential.Resolver (StaticResolver)                  │
│  职责: 登录、Token 存储、Token 刷新、注入 token 到 Runtime     │
│                                                              │
│  定义于: AgentKit Credential Model v1                        │
└───────────────────────┬──────────────────────────────────────┘
                        │ secretsJSON / Reconfigure
┌───────────────────────▼──────────────────────────────────────┐
│  Code-Agent Runtime                                          │
│                                                              │
│  持有: credential.Resolver（宿主注入，Runtime 不知道来源）         │
│  使用: OpenAICompatibleProvider → HTTP POST /chat/completions │
│  不知道: Gateway / TokenProvider / OAuth / Keychain
  不实现: token 刷新、refresh_token 存储、/auth/refresh        │
│                                                              │
│  定义于: 本文件                                              │
└──────────────────────────────────────────────────────────────┘
```

---

## 9. 实现检查清单

### 9.1 Runtime 新增

- [ ] `internal/credential/` 包
  - [ ] `Resolver` 接口
  - [ ] `Target`, `Credential`, `ResolvedCredential`
  - [ ] `EnvResolver`, `StaticResolver`, `ChainResolver`
  - [ ] `FileResolver`（CLI login 持久化，可延后）
- [ ] `internal/model/openai_compatible.go`
  - [ ] `Credential credential.Resolver` 字段
  - [ ] `CredentialTarget credential.Target` 字段
  - [ ] 保留 `APIKey` 兼容路径
- [ ] `internal/runtime/provider.go`
  - [ ] `BuildProvider` 接收 `credential.Resolver`
- [ ] `internal/embed/server.go`
  - [ ] `injectSecrets` 创建 `StaticResolver` 加入 chain
- [ ] 401 错误传播路径验证
  - [ ] `ResilientProvider.isRetryable(401)` → `false` ✅ 已是正确行为
  - [ ] `TurnExecutor` 在 turn_failed 事件中携带 auth_expired 信息

### 9.2 Runtime 不新增

- [ ] ❌ `/auth/refresh` 调用
- [ ] ❌ `refresh_token` 存储
- [ ] ❌ `gateway.TokenProvider` — 不需要，`StaticResolver` 已足够
- [ ] ❌ `internal/gateway/` 包 — Runtime 不知道 Gateway 存在
- [ ] ❌ OAuth 流程
- [ ] ❌ 独立的 `GatewayProvider` 类型

### 9.3 宿主层（由 AgentKit/CLI 团队实现）

- [ ] Token 刷新逻辑
- [ ] Keychain/文件存储
- [ ] 构造 `StaticResolver` 注入 gateway token
- [ ] 401 事件监听 → 刷新 → `Reconfigure`

---

## 附录 A：宿主注入示例

### A.1 AgentKit 侧（Swift 伪代码）

```swift
// AgentKit 构造 StaticResolver 的 credential 数据，通过 secretsJSON 注入
func buildSecretsJSON(accessToken: String, expiresAt: TimeInterval) -> String {
    return """
    {
      "gateway/default": {
        "type": "bearer",
        "secret": "\(accessToken)",
        "expires_at": \(Int(expiresAt))
      }
    }
    """
}

// 启动时注入
let secrets = buildSecretsJSON(accessToken: keychain.token, expiresAt: keychain.expiry)
let server = try mobile.start(workspaceDir, dataDir, configYAML, modelName, secrets, addr, false)

// Token 过期时刷新并热切换
func handleTokenExpired() async throws {
    let newToken = try await authClient.refresh()
    keychain.save(newToken)
    let secrets = buildSecretsJSON(accessToken: newToken.accessToken, expiresAt: newToken.expiry)
    try server.reconfigure(secrets, modelName: "")
}
```

### A.2 CLI 侧（Go 伪代码）

```go
// CLI 构造包含 gateway token 的 StaticResolver
func buildGatewayResolver(token string) credential.Resolver {
    target := credential.Target{Namespace: "gateway", Name: "default"}
    return credential.StaticResolver{
        target: {
            Type:   credential.Bearer,
            Secret: token,
        },
    }
}

// CLI 使用 FileResolver 读取持久化的 credential
func buildCLIChain(homeDir string) credential.Resolver {
    return &credential.ChainResolver{
        Resolvers: []credential.Resolver{
            credential.EnvResolver{},                     // --gateway-token 或其他 env
            credential.NewFileResolver(filepath.Join(homeDir, ".codeagent", "credentials")),
        },
    }
}
```

### A.3 为什么不需要 TokenProvider

```go
// ❌ 不需要：
type TokenProvider interface {
    Token(ctx context.Context) (string, error)
}

// ✅ 已足够：
resolver := credential.StaticResolver{
    {Namespace: "gateway", Name: "default"}: {
        Type:   credential.Bearer,
        Secret: token,
    },
}
// Runtime 通过 credential.Resolver.Resolve() 获取，不知道背后是 Keychain 还是环境变量。
```
```

---

## 附录 B：与 Agent Gateway API 契约的对应关系

| Gateway API (agent-gateway-api-v1.md) | Runtime 集成 (本文件) |
|---------------------------------------|----------------------|
| `POST /api/v1/auth/login` | Runtime 不调用 — 宿主负责 |
| `POST /api/v1/auth/refresh` | Runtime 不调用 — 宿主负责 |
| `POST /api/v1/agent/chat/completions` | `OpenAICompatibleProvider.Complete()` |
| `Authorization: Bearer <token>` | `credential.Resolver.Resolve()` → `Credential.Secret` → HTTP header |
| HTTP 401 → 宿主刷新 | `ResilientProvider` 不重试 → `turn_failed` → 宿主刷新 |
| `POST /api/v1/agent/chat/completions` (流式) | `OpenAICompatibleProvider.CompleteStream()` |
| Usage 追踪 | Gateway 服务端；Runtime 的可选 `Observer` 记录本地估算 |
