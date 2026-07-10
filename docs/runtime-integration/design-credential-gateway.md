# Code-Agent Runtime — Credential & Gateway Integration Design

> **状态**: 设计阶段，未进入实现
> **受众**: Runtime 开发 + AgentKit 开发 + 架构评审
> **版本**: v2.0 — 合并 AgentKit 评审反馈
> **日期**: 2026-07-09

---

## 0. 现有架构分析

### 0.1 当前 Credential 流

```
CLI 场景:
  env var → LoadConfig → ModelConfig.APIKey (string) → BuildProvider → Provider.APIKey (string) → HTTP Authorization header

Embedded 场景 (iOS/macOS):
  Keychain → Secrets map → injectSecrets → ModelConfig.APIKey (string) → BuildProvider → Provider.APIKey (string) → HTTP Authorization header
```

### 0.2 当前关键类型

```go
// model/types.go — 纯接口，无认证概念
type Provider interface {
    Complete(ctx context.Context, request Request) (Response, error)
}

// model/openai_compatible.go — API key 是 struct field，构造时写入
type OpenAICompatibleProvider struct {
    BaseURL    string
    APIKey     string          // ← 静态字符串，无法过期/刷新
    HTTPClient *http.Client
}

// model/resilient.go — 纯传输层包装
type ResilientProvider struct {
    Inner      Provider
    MaxRetries int
    Timeout    time.Duration
}

// internal/app/config.go — key 在 load 时 resolve
type ModelConfig struct {
    Provider  string  // "openai" | "ollama"
    BaseURL   string
    APIKeyEnv string  // env var 名
    APIKey    string  `yaml:"-"` // ← resolve 后的值
}

// internal/embed/server.go — 外部注入
func injectSecrets(cfg *app.Config, secrets map[string]string)
```

### 0.3 现有架构的优点

1. **`model.Provider` 接口纯净** — `Complete(ctx, req) (Response, error)` 不涉及任何认证
2. **密钥注入路径已存在** — `injectSecrets` + `Reconfigure` = AgentKit → Runtime credential 通道
3. **`ResilientProvider` 是干净的传输层** — 重试/超时/退避与认证完全解耦
4. **gomobile 边界已定义** — `mobile.Start()` 接收 `secretsJSON`，Runtime 不知道 Keychain

### 0.4 当前架构的缺口

1. **API key 是静态字符串** — 无法支持 JWT 过期/刷新（Gateway、MCP OAuth）
2. **Provider 构造时绑定 credential** — 无法每次请求动态获取
3. **无 Gateway 概念** — 所有 provider 直连模型 API
4. **credential 来源与 provider 紧耦合** — `api_key_env` 绑定在 model 配置下

---

## 1. 最终架构总览

```
                 AgentKit
              (商业客户端层)
        Login
        Subscription
        Keychain
        Usage UI
             |
             |
        Credential Injection
        (secretsJSON + Reconfigure)
             |
             ↓
          Code-Agent Runtime
          (开源核心)
     ┌─────────────────────────┐
     │  credential/            │
     │                         │
     │  Resolver  interface    │
     │  Chain                  │
     │  EnvResolver            │
     │  StaticResolver         │
     │  CachedResolver         │
     └───────────┬─────────────┘
                 │
     ┌───────────┴─────────────┐
     │                         │
  Gateway               BYOK
  (JWT Bearer)         (API Key)
     │                     │
  Agent Gateway        DeepSeek/OpenAI
  /chat/completions    /v1/chat/completions
```

### 1.1 责任边界

| 能力 | AgentKit | Runtime |
|------|----------|---------|
| 登录 | ✅ | ❌ |
| JWT 保存 | ✅ Keychain | ❌ |
| 刷新 token | ✅ | ❌ |
| 订阅状态 | ✅ | ❌ |
| Quota UI | ✅ | ❌ |
| Credential 存储 | ✅ | ❌ |
| Credential 消费 | ❌ | ✅ |
| Provider 调用 | ❌ | ✅ |
| Agent Loop | ❌ | ✅ |
| Tool 执行 | ❌ | ✅ |
| MCP OAuth 流程 | 部分（用户授权 UI） | 部分（token 交换） |

### 1.2 红线

```
禁止出现在 Runtime 核心代码中的 import:
  ❌ "auth"
  ❌ "keychain"
  ❌ "billing"
  ❌ "subscription"
  ❌ "appleid"

禁止出现在 Runtime 核心代码中的方法:
  ❌ runtime.Login()
  ❌ runtime.Register()
  ❌ runtime.SaveToken()
  ❌ runtime.RefreshToken()
```

---

## 2. Credential 核心抽象

> **命名决策**: 使用 `Resolver` 而非 `Provider`，避免与 `model.Provider` 混淆。
> 使用 `Namespace` 而非 `Type`，避免与 `Credential.Type` 混淆。

### 2.1 核心类型

```go
// Package credential 定义了 Runtime 获取服务凭证的抽象。
// Runtime 只知道 "我需要某个服务的 credential"，不知道 credential 从哪来。
//
// 这是开源 core 的一部分 — 零外部依赖，仅标准库。
package credential

import (
    "context"
    "time"
)

// Target 唯一标识一个需要凭证的服务。
//
// Namespace 是服务类别（"gateway"、"llm"、"mcp"）。
// Name 是具体实例（"default"、"deepseek"、"github"）。
//
// 为什么叫 Namespace 而不是 Type：
//   Credential 自身也有 Type 字段（"bearer"、"api_key"、"oauth2"），
//   如果 Target 也用了 Type，代码中到处都是 "Type" 会产生不可接受的歧义。
type Target struct {
    Namespace string // "gateway" | "llm" | "mcp"
    Name      string // "default" | "deepseek" | "github"
}

// CredentialType 是凭证在 HTTP 层的传输类型。
// 它只描述 "如何放入 HTTP Header"，不描述 "凭证来自什么认证协议"。
//
// 为什么没有 OAuth2 类型：
//   OAuth2 是认证协议，不是 wire format。OAuth2 access token 在 HTTP 层
//   仍然是 Authorization: Bearer <token>。Runtime 不关心 token 是通过
//   OAuth2、OIDC、SAML 还是手动输入获得的 — 它只关心 header 怎么写。
//   refresh_token 等协议细节放在 Metadata 中。
type CredentialType string

const (
    // Bearer — Authorization: Bearer <Secret>（JWT、OAuth2 access token、API key）
    Bearer CredentialType = "bearer"
    // Secret — 非 Bearer 的机密凭证（未来扩展：AWS SigV4、mTLS client cert 等）
    Secret CredentialType = "secret"
    // None — 无需凭证（本地模型）
    None CredentialType = "none"
)

// Credential 是访问某个服务所需的凭证。
// 它是值对象，不包含刷新逻辑。
//
// 为什么叫 Secret 而不是 Value：
//   Value 太泛，无法表达 "这是不可展示的敏感数据" 的语义。
//   Secret 明确表示 jwt / apikey / oauth token 都属于机密信息。
//   未来可能扩展 Certificate、PrivateKey 等字段，Secret 的语义边界更清晰。
type Credential struct {
    Type   CredentialType // bearer | secret | none
    Secret string         // token 或 API key — 不可展示的机密数据

    // ExpiresAt 是可选的过期时间。nil 表示永不过期（如静态 API key）。
    // CachedResolver 使用此字段判断是否需要刷新。
    ExpiresAt *time.Time

    // Metadata 携带额外的凭证上下文，如 refresh_token、scope 等。
    // Resolver 实现可以在此存储任意 key-value，消费方应只读取已知 key。
    Metadata map[string]string
}

// ResolvedCredential 是 Resolver 的返回值，在 Credential 基础上附加来源信息。
// Source 用于 debug / 日志 / 审计：当用户问 "为什么用的是这个 key？"
// 时，可以从 Source 追溯到具体的 Resolver。
type ResolvedCredential struct {
    Credential
    Source string // Resolver 的名称，如 "env:DEEPSEEK_API_KEY"、"chain[0]=env"、"agentkit:gateway"
}

// IsZero 判断是否为空凭证。
func (c Credential) IsZero() bool {
    return c.Type == "" && c.Secret == ""
}

// IsExpired 判断凭证是否已过期。nil ExpiresAt 表示永不过期。
func (c Credential) IsExpired() bool {
    if c.ExpiresAt == nil {
        return false
    }
    return time.Now().After(*c.ExpiresAt)
}
```

### 2.2 Resolver 接口

```go
// Resolver 是凭证的来源。它回答 "对于这个 Target，你有什么凭证？"。
//
// 实现者：
//   - EnvResolver      — 从环境变量读取（CLI）
//   - StaticResolver    — 固定凭证（配置注入、测试）
//   - ChainResolver     — 按顺序尝试多个 Resolver
//   - CachedResolver    — 包装另一个 Resolver，缓存并自动刷新
//
// 约定（参考 AWS SDK v2 credential chain）：
//   - 无法处理此 Target  → 返回 (ResolvedCredential{}, nil)，不是 error
//   - 能处理但失败了      → 返回 (ResolvedCredential{}, error)
//   - 调用方遍历 chain    → 零值 = 跳过，error = 是否短路（取决于 chain 配置）
//
// Source 字段用于可追溯性：
//   EnvResolver       → "env:DEEPSEEK_API_KEY"
//   StaticResolver    → "static"
//   ChainResolver     → "chain[1]=env:DEEPSEEK_API_KEY"
//   CachedResolver    → "cached(env:DEEPSEEK_API_KEY)"
//   AgentKit 注入      → "agentkit:gateway"
type Resolver interface {
    Resolve(ctx context.Context, target Target) (ResolvedCredential, error)
}
```

### 2.3 为什么叫 Resolver？

```
现有命名空间冲突：

model.Provider   ← 已存在，Complete(ctx, req) (Response, error)
credential.Provider ← 如果也叫 Provider，代码中会看到:
                       provider.Provider vs credential.Provider
                       调用处不可区分，必须靠 import alias

改为 Resolver 后:
    model.Provider       → 提供 LLM 推理服务
    credential.Resolver  → 提供访问凭证
    语义精确，零歧义
```

### 2.4 内置实现

```go
// EnvResolver 从环境变量获取凭证。
//
// 默认映射规则（可由 Mapping 覆盖）：
//   {Namespace:"llm",  Name:"deepseek"}  → DEEPSEEK_API_KEY
//   {Namespace:"llm",  Name:"openai"}    → OPENAI_API_KEY
//   {Namespace:"mcp", Name:"github"}    → GITHUB_TOKEN
//
// EnvResolver 返回的 Credential.ExpiresAt 为 nil（永不过期），Source 为 "env:<VAR_NAME>"。
type EnvResolver struct {
    Mapping map[Target]string // Target → env var 名，nil 则使用默认规则
}

func (r *EnvResolver) Resolve(ctx context.Context, t Target) (ResolvedCredential, error)

// StaticResolver 返回预配置的固定凭证。用于配置注入和测试。
//
// StaticResolver 返回的 Credential 原样返回，包括 ExpiresAt。
type StaticResolver map[Target]Credential

func (r StaticResolver) Resolve(ctx context.Context, t Target) (ResolvedCredential, error)

// ChainResolver 按顺序尝试多个 Resolver。
//
// 行为：
//   - 返回第一个非零 Credential
//   - 单个 Resolver 返回 error 时的行为由 FailFast 控制
//   - 所有 Resolver 都返回零值时返回零值
type ChainResolver struct {
    Resolvers []Resolver
    FailFast  bool // true: 任意 error 短路；false: 跳过继续
}

func (r *ChainResolver) Resolve(ctx context.Context, t Target) (ResolvedCredential, error)

// CachedResolver 包装一个 Resolver，缓存 credential。
//
// 刷新策略：
//   - credential 未过期 → 直接返回缓存
//   - credential 在 RefreshWindow 内将过期 → 同步刷新（阻塞直到刷新完成）
//   - credential 已过期 → 同步刷新
//
// 如果 Inner 返回的 Credential.ExpiresAt 为 nil，CachedResolver 退化为
// 永久缓存（TTL 控制最大缓存时间）。
type CachedResolver struct {
    Inner         Resolver
    TTL           time.Duration // 最大缓存时间（ExpiresAt 优先）
    RefreshWindow time.Duration // 过期前多久触发刷新
    Now           func() time.Time // 可注入的时钟（测试用）
}

func (r *CachedResolver) Resolve(ctx context.Context, t Target) (ResolvedCredential, error)
```

### 2.5 ResolvedCredential 示例

```go
// Gateway JWT
credential.ResolvedCredential{
    Credential: credential.Credential{
        Type:      credential.Bearer,
        Secret:    "eyJhbGciOiJSUzI1NiIs...",
        ExpiresAt: &time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC),
    },
    Source: "agentkit:gateway",
}

// BYOK API Key — wire format 也是 Bearer
credential.ResolvedCredential{
    Credential: credential.Credential{
        Type:      credential.Bearer,
        Secret:    "sk-abc123...",
        // ExpiresAt: nil — 永不过期
    },
    Source: "env:DEEPSEEK_API_KEY",
}

// 未来：MCP OAuth — wire format 仍然是 Bearer，协议细节在 Metadata
credential.ResolvedCredential{
    Credential: credential.Credential{
        Type:      credential.Bearer,
        Secret:    "gho_abc123...",
        ExpiresAt: &time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
        Metadata: map[string]string{
            "protocol":       "oauth2",          // ← 认证协议，不影响 header
            "provider":       "github",
            "refresh_token":  "ghr_xyz789...",
            "scope":          "repo,user",
        },
    },
    Source: "mcp:github-oauth",
}
```

---

## 3. OpenAICompatibleProvider 改造

### 3.1 设计决策

**不要**创建单独的 `GatewayProvider`。Gateway 本质上就是：
- OpenAI-compatible endpoint (`/chat/completions`)
- + JWT credential（而非 API key）

唯一的区别是 credential target 不同。同一个 `OpenAICompatibleProvider` 实例通过不同的 credential 配置来区分行为。

### 3.2 改造后

```go
// model/openai_compatible.go

type OpenAICompatibleProvider struct {
    BaseURL          string
    Credential       credential.Resolver   // ← 不再持有静态 APIKey
    CredentialTarget credential.Target     // ← 解析时传给 Resolver
    HTTPClient       *http.Client
}

// NewOpenAICompatibleProvider 创建带动态 credential 的 provider。
// cred 为 nil 时表示无需认证（本地模型或 HTTPClient.Transport 自行处理）。
func NewOpenAICompatibleProvider(baseURL string, cred credential.Resolver, target credential.Target) *OpenAICompatibleProvider {
    return &OpenAICompatibleProvider{
        BaseURL:          strings.TrimRight(baseURL, "/"),
        Credential:       cred,
        CredentialTarget: target,
        HTTPClient:       defaultHTTPClient(),
    }
}
```

### 3.3 请求时的 credential 注入

```go
func (p *OpenAICompatibleProvider) applyAuth(ctx context.Context, req *http.Request) error {
    if p.Credential == nil {
        return nil // 无需认证
    }
    c, err := p.Credential.Resolve(ctx, p.CredentialTarget)
    if err != nil {
        return fmt.Errorf("resolve credential for %v: %w", p.CredentialTarget, err)
    }
    if c.IsZero() {
        return nil // Resolver 不负责此 target
    }
    // c.Source 可用于 debug 日志："using credential from env:DEEPSEEK_API_KEY"
    switch c.Type {
    case credential.Bearer:
        req.Header.Set("Authorization", "Bearer "+c.Secret)
    case credential.Secret:
        // 非 Bearer 凭证 — 由 HTTPClient.Transport 自行处理（如 AWS SigV4 signer）
    case credential.None:
        // 无需认证
    }
    return nil
}
```

### 3.4 向后兼容

```go
// NewOpenAICompatibleProviderWithKey 是向后兼容的构造函数。
//
// Deprecated: 新代码应使用 NewOpenAICompatibleProvider(baseURL, cred, target)。
// 此方法将在后续大版本移除。
func NewOpenAICompatibleProviderWithKey(baseURL, apiKey string) *OpenAICompatibleProvider {
    var cred credential.Resolver
    var target credential.Target
    if apiKey != "" {
        target = credential.Target{Namespace: "llm", Name: "default"}
        cred = credential.StaticResolver{
            target: {Type: credential.Bearer, Secret: apiKey},
        }
    }
    return NewOpenAICompatibleProvider(baseURL, cred, target)
}
```

### 3.5 Gateway vs BYOK — 同一个类型，不同配置

```go
// Gateway 模式
gatewayProvider := model.NewOpenAICompatibleProvider(
    "https://agent.xxx.com/api/v1/agent",
    gatewayResolver,
    credential.Target{Namespace: "gateway", Name: "default"},
)

// BYOK 模式
byokProvider := model.NewOpenAICompatibleProvider(
    "https://api.deepseek.com",
    envResolver,
    credential.Target{Namespace: "llm", Name: "deepseek"},
)

// 两者是同一个类型，只是 credential target 不同。
// Agent Loop 不感知这个区别。
```

---

## 4. BuildProvider 改造

```go
// internal/runtime/provider.go

func BuildProvider(mc app.ModelConfig, pc app.ProviderConfig, cred credential.Resolver) (model.Provider, error) {
    target := credential.Target{
        Namespace: mc.Credential.Namespace,
        Name:      mc.Credential.Name,
    }
    if target.Namespace == "" {
        // 旧格式兼容：有 api_key_env 的 model 默认为 llm namespace
        target.Namespace = "llm"
        target.Name = mc.Name
    }

    var inner model.Provider
    switch mc.Provider {
    case "openai", "gateway", "":
        // Gateway 与 OpenAI-compatible 使用相同的 provider 实现。
        // 区别仅在于 CredentialTarget 不同。
        inner = model.NewOpenAICompatibleProvider(mc.BaseURL, cred, target)
    case "ollama":
        inner = model.NewOllamaProvider(mc.BaseURL)
    default:
        return nil, fmt.Errorf("unsupported provider %q", mc.Provider)
    }
    return &model.ResilientProvider{
        Inner:      inner,
        MaxRetries: pc.MaxRetries,
        Timeout:    time.Duration(pc.RequestTimeoutSeconds) * time.Second,
        Backoff:    time.Duration(pc.BackoffMillis) * time.Millisecond,
        MaxBackoff: time.Duration(pc.MaxBackoffSeconds) * time.Second,
    }, nil
}
```

关键点：`provider: gateway` 不是单独的 case — 它和 `openai` 走同一条路径，区别仅在于 `CredentialTarget.Namespace` 不同。

---

## 5. 配置设计

### 5.1 当前配置

```yaml
models:
  deepseek:
    provider: openai
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-flash"
    api_key_env: DEEPSEEK_API_KEY      # ← credential 与 model 绑定
```

### 5.2 目标配置

```yaml
# ── Credential 来源定义（独立 section）──
credentials:
  gateway:
    source: external                     # AgentKit 注入（或 CLI --gateway-token）

  deepseek:
    source: env
    env: DEEPSEEK_API_KEY

  openai:
    source: env
    env: OPENAI_API_KEY

  ollama:
    source: none

# ── Model 定义 ──
models:
  # Gateway 路由 — 模型选择在 Gateway 服务端
  deepseek:
    provider: openai                    # 底层协议
    base_url: "https://agent.xxx.com/api/v1/agent"
    model: "deepseek-v4-flash"
    credential:
      namespace: gateway
      name: default

  # BYOK 直连
  deepseek-byok:
    provider: openai
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-flash"
    credential:
      namespace: llm
      name: deepseek

  # 本地模型
  ollama-coder:
    provider: ollama
    base_url: "http://localhost:11434"
    model: "qwen3-coder-tool"
    # 无 credential section = 无需认证
```

### 5.3 向后兼容

旧格式继续有效。`api_key_env` 等价于隐式的 `credential: {namespace: llm, name: <model_name>}`：

```yaml
# 旧格式（继续支持）
models:
  deepseek:
    provider: openai
    base_url: "https://api.deepseek.com"
    model: "deepseek-v4-flash"
    api_key_env: DEEPSEEK_API_KEY

# LoadConfig 中的等价转换:
#   if mc.Credential.Namespace == "" && mc.APIKeyEnv != "" {
#       mc.Credential = CredentialRef{Namespace: "llm", Name: name}
#   }
```

### 5.4 ModelConfig 扩展

```go
type ModelConfig struct {
    Provider    string  `yaml:"provider"`
    BaseURL     string  `yaml:"base_url"`
    Model       string  `yaml:"model"`
    APIKeyEnv   string  `yaml:"api_key_env"`   // 旧格式，继续支持
    Temperature float64 `yaml:"temperature"`
    ContextWindow int   `yaml:"context_window"`
    // ...

    // Credential 显式引用 credentials section（新格式）。
    // 为空时回退到 api_key_env 的行为。
    Credential CredentialRef `yaml:"credential"`

    // Resolved at load time.
    Name   string `yaml:"-"`
    APIKey string `yaml:"-"` // 旧格式 resolve 后的值，逐步废弃
}

// CredentialRef 指向 credentials section 中的一项。
type CredentialRef struct {
    Namespace string `yaml:"namespace"`
    Name      string `yaml:"name"`
}
```

---

## 6. Credential Chain 设计

### 6.1 Chain 服务于所有场景

Credential Chain 不止服务 AgentKit。它服务四种场景：

```
                    ┌─────────────────────────────┐
                    │     Credential Chain         │
                    │                              │
                    │  1. InjectedResolver         │ ← AgentKit（外部注入）
                    │  2. EnvResolver              │ ← CLI / CI / Server
                    │  3. FileResolver             │ ← ~/.codeagent/credentials
                    │  4. ConfigResolver           │ ← config.yaml credentials
                    │  5. StaticResolver           │ ← 默认/fallback
                    └─────────────────────────────┘
```

### 6.2 各场景的 Chain 配置

**CLI 场景**：

```go
chain := &credential.ChainResolver{
    Resolvers: []credential.Resolver{
        credential.EnvResolver{},            // DEEPSEEK_API_KEY 等
        credential.NewFileResolver(homeDir), // ~/.codeagent/credentials
    },
}
```

对应：`export DEEPSEEK_API_KEY=xxx`

**CLI 登录场景**（未来）：

```bash
codeagent login
# → 打开浏览器
# → OAuth 登录 Agent Gateway
# → 保存 JWT 到 ~/.codeagent/credentials
# → CLI 继续使用 FileResolver 读取
```

```
codeagent login 流程:
  CLI 启动本地 HTTP server (localhost 临时端口)
  → 打开浏览器到 https://agent.xxx.com/authorize?...
  → 用户在浏览器完成 OAuth
  → Gateway 回调 localhost → CLI 收到 JWT
  → 写入 ~/.codeagent/credentials
  → FileResolver 随后的请求自动读取
```

**AgentKit 场景**：

```go
chain := &credential.ChainResolver{
    Resolvers: []credential.Resolver{
        injectedResolver,                    // AgentKit 注入（优先级最高）
        credential.EnvResolver{},            // fallback: 用户可能设了 env
    },
}
```

**CI/Server 场景**：

```go
chain := &credential.ChainResolver{
    Resolvers: []credential.Resolver{
        credential.EnvResolver{},            // CI secrets → env vars
        credential.NewFileResolver("/etc/codeagent/credentials"),
    },
}
```

### 6.3 AgentKit 注入实现

**不**跨 gomobile bridge Go interface。使用已有的 `secretsJSON` + `Reconfigure` 通道：

```
AgentKit (Swift)                        Runtime (Go)
─────────────                          ────────────
Keychain                               
  ↓                                    
accessToken                           
  ↓                                    
JSON: {                                
  "gateway/default": {                 
    "type": "bearer",                  
    "secret": "eyJ...",                 
    "expires_at": 1234567890           
  }                                    
}                                      
  ↓ secretsJSON                        
  ────────────── gomobile ──────────→  
                                       injectSecrets(cfg, secrets)
                                         ↓
                                       创建 StaticResolver
                                         ↓
                                       加入 ChainResolver
                                         ↓
                                       BuildProvider(mc, pc, chain)
```

Token 刷新时 AgentKit 调用 `Reconfigure`：

```go
// mobile/mobile.go
func (s *Server) Reconfigure(secretsJSON, modelName string) error {
    // 现有方法已支持。新增的是 secretsJSON 中的 credential 格式。
    // 旧的 "DEEPSEEK_API_KEY": "sk-..." 格式继续支持
    // 新的 "gateway/default": {"type":"bearer","secret":"...","expires_at":...} 格式
    // 被 injectSecrets 解释为 credential 注入
}
```

**为什么不用 Go interface 跨 gomobile？**

1. gomobile 不支持 bridge Go interface
2. credential 刷新是 AgentKit 的 timer/push 驱动，和 Runtime 的 request/pull 驱动是不同调度域
3. 推送式（AgentKit push → Reconfigure → Runtime）比拉取式（Runtime pull → AgentKit）更简单可靠
4. 已经在用了 — `secretsJSON` + `Reconfigure` 就是这个模式，只是需要扩展数据格式

---

## 7. CLI 登录设计（未来）

### 7.1 问题

当前 CLI 用户只能通过环境变量或配置文件提供 API key。引入 Gateway 后，CLI 用户也需要登录。

对标 Claude Code：
```
claude login
→ 打开浏览器
→ OAuth
→ 保存 credential
→ claude 后续请求自动携带
```

### 7.2 设计

```bash
codeagent login
```

流程：

1. CLI 在 `localhost` 随机端口启动临时 HTTP server
2. 打开系统浏览器到 `https://agent.xxx.com/authorize?redirect_uri=http://localhost:<port>/callback&...`
3. 用户在浏览器完成 OAuth / 登录
4. Gateway 回调 `localhost:<port>/callback?code=...`
5. CLI 用 code 换取 JWT（access_token + refresh_token）
6. 写入 `~/.codeagent/credentials`：

```json
{
  "gateway/default": {
    "type": "bearer",
    "secret": "eyJhbGciOi...",
    "expires_at": "2026-07-10T09:00:00Z",
    "metadata": {
      "refresh_token": "rt_xxx..."
    }
  }
}
```

7. `FileResolver` 在后续请求中自动读取并提供此 credential
8. `CachedResolver` 包装 `FileResolver`，在过期前自动刷新

### 7.3 FileResolver

```go
// FileResolver 从磁盘文件读取 credential。
// 文件格式：{ "<namespace>/<name>": { "type": "...", "secret": "...", ... } }
//
// 用于：
//   - CLI 登录后的持久化 credential（~/.codeagent/credentials）
//   - CI/Server 的预配置 credential（/etc/codeagent/credentials）
type FileResolver struct {
    Path string
}

func NewFileResolver(path string) *FileResolver

func (r *FileResolver) Resolve(ctx context.Context, t Target) (ResolvedCredential, error)
```

---

## 8. 搜索/MCP Credential 策略

### 8.1 Web Search

**不要在 credential package 中创建 `search/tavily` target。**

原因：这是 provider 细节泄露到 credential 层。正确的分层是：

```
Gateway 模式（推荐）：
  Runtime → Gateway → Search Service → Tavily/Google/Bing
  Runtime 只知道 gateway/default credential，不知道背后是哪个搜索提供商

BYOK 模式（用户自备 Key）：
  用户通过 env 提供 TAVILY_API_KEY
  EnvResolver 读取
  但这不是 credential.Target{Namespace:"search", Name:"tavily"}
  而是直接注入到 WebSearchConfig.TavilyKey（现有机制）
```

现有 `WebSearchConfig.TavilyKey` 注入路径保持不变。

### 8.2 MCP OAuth（未来）

MCP OAuth 是 credential 抽象的天然扩展场景：

```go
// 未来 MCP Manager 连接 MCP server 时：
func (m *Manager) Connect(ctx context.Context, servers []ServerConfig, cred credential.Resolver) error {
    for _, s := range servers {
        if s.CredentialTarget != nil {
            c, err := cred.Resolve(ctx, *s.CredentialTarget)
            // c.Type == credential.Bearer
            // c.Metadata["protocol"] == "oauth2"
            // c.Metadata["refresh_token"] 可用于 token 刷新
        }
    }
}
```

对应的 `.mcp.json` 配置：

```json
{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.github.com/mcp",
      "credential": {
        "namespace": "mcp",
        "name": "github"
      }
    }
  }
}
```

---

## 9. 开源版本边界

### 9.1 分层

```
┌──────────────────────────────────────────────┐
│  Agent Gateway Service (商业化)               │
│  - 用户管理 / 订阅 / 计费                      │
│  - 模型路由 / Usage 追踪                       │
│  - OAuth Provider                            │
└──────────────────┬───────────────────────────┘
                   │ HTTPS /chat/completions
┌──────────────────▼───────────────────────────┐
│  AgentKit (商业化 UI，Swift)                  │
│  - 登录 / 注册 / Keychain                     │
│  - Token 刷新 / Subscription 展示             │
│  - 注入 credential 到 Runtime                │
└──────────────────┬───────────────────────────┘
                   │ gomobile (secretsJSON)
┌──────────────────▼───────────────────────────┐
│  Code-Agent Runtime (开源 core，Go)          │
│                                               │
│  credential/          ← 零外部依赖             │
│  model/               ← credential 是可选字段   │
│  app/                 ← 构造 ChainResolver    │
│  runtime/             ← BuildProvider 接收    │
│                          credential.Resolver  │
└──────────────────────────────────────────────┘
```

### 9.2 开源 core 包含

| 组件 | 说明 |
|------|------|
| `credential.Resolver` 接口 | 核心抽象，零外部依赖 |
| `credential.Target` / `Credential` | 值对象 |
| `EnvResolver` | CLI / CI 场景 |
| `StaticResolver` | 配置注入 + 测试 |
| `ChainResolver` | 组合多个来源 |
| `CachedResolver` | 缓存 + 过期刷新 |
| `FileResolver` | CLI login 持久化 |
| `OpenAICompatibleProvider` (+Credential/ CredentialTarget) | 改造后的 provider |
| Gateway 配置支持 | `provider: openai` + `credential.namespace: gateway` |
| `codeagent login` | CLI OAuth 登录（未来） |

### 9.3 开源 core 不包含

| 组件 | 所在位置 |
|------|---------|
| Gateway JWT 刷新逻辑 | AgentKit |
| 用户登录/注册 | AgentKit + Gateway Service |
| 订阅管理 | AgentKit + Gateway Service |
| Keychain 存储 | AgentKit（macOS/iOS 原生） |
| OAuth UI | AgentKit |

### 9.4 模块依赖方向

```
credential/  ← 零外部依赖，仅标准库
    ↑
model/       ← 依赖 credential/ (Credential, Resolver, Target 是可选字段)
    ↑
app/         ← 依赖 credential/ (构造 ChainResolver)
    ↑
runtime/     ← 依赖 model/ + app/
    ↑
embed/       ← 依赖 runtime/
    ↑
mobile/      ← 依赖 embed/ (gomobile boundary)
```

开源 core 编译时不需要 AgentKit 或任何商业化组件。

---

## 10. 实现路线图

### Phase A：核心抽象（零业务影响）

创建 `internal/credential/` 包，不改任何现有代码：

1. `types.go` — `Target`, `Credential`, `CredentialType`
2. `resolver.go` — `Resolver` 接口
3. `env.go` — `EnvResolver`
4. `static.go` — `StaticResolver`
5. `chain.go` — `ChainResolver`
6. `cached.go` — `CachedResolver`（可选，可晚于 Phase A）
7. 全部单元测试

### Phase B：Provider 兼容改造

3. `OpenAICompatibleProvider` 加 `Credential`/`CredentialTarget` 字段
   - 保留 `APIKey` 字段和 `NewOpenAICompatibleProviderWithKey` 兼容构造函数
   - `applyAuth()` 优先走 `Resolver`，回退到 `APIKey`

4. `BuildProvider` 新增 `cred credential.Resolver` 参数
   - 现有调用方传 `nil`（行为不变）

### Phase C：配置系统

5. 扩展 `ModelConfig` — `CredentialRef` 字段
6. 扩展 `LoadConfig` — 解析 `credential` section
7. 构造 `ChainResolver`

### Phase D：AgentKit 集成

8. 扩展 `injectSecrets` 支持 credential 格式
9. `Reconfigure` 支持 credential 注入

### Phase E：CLI 登录（未来）

10. `FileResolver`
11. `codeagent login` 命令
12. Gateway OAuth 回调处理

---

## 11. 参考

| 项目 | 核心模式 | 借鉴点 |
|------|---------|--------|
| **AWS SDK v2** | `Provider` → `Retrieve()` → `Credentials` | 零值 = "not my target" 语义、Chain 模式 |
| **Anthropic Go SDK** | 5 层 credential chain + middleware | 多层解析、caller-supplied 可覆盖任何源 |
| **OpenAI Go SDK** | `TokenRoundTripper` + `WithMiddleware` | HTTP 层注入，Provider 不感知认证 |
| **Vercel AI SDK** | Gateway 统一端点 + OIDC | Gateway 对 Runtime 暴露标准 `/chat/completions` |
| **Claude Code** | `claude login` → OAuth → 保存 | CLI 登录流程参考 |
| **Cline** | Provider registry + Handler factory | 配置驱动、credential 外部注入 |
| **Copilot SDK** | NamedProvider + ProviderModelConfig + BYOK chain | 多 provider/multi-model |

---

## 12. 评审检查清单

### 12.1 Runtime 污染检查

- [ ] `internal/credential/` 无 `auth`、`keychain`、`billing`、`subscription` import
- [ ] `internal/model/` 无登录/注册方法
- [ ] `internal/runtime/` 无 token 持久化逻辑
- [ ] `internal/app/config.go` 无 subscription tier 字段

### 12.2 命名歧义检查

- [ ] `model.Provider` 和 `credential.Resolver` 不会在同一个文件中产生歧义
- [ ] `Target.Namespace` 和 `Credential.Type` 不会混淆

### 12.3 Credential 可扩展性

- [ ] Gateway JWT bearer token ✅
- [ ] BYOK API key ✅
- [ ] MCP OAuth（设计层面）✅ — `Credential.Type = "oauth2"` + `Metadata`
- [ ] Enterprise SSO（设计层面）✅ — `Target.Namespace = "enterprise"` + 自定义 Resolver
- [ ] `CredentialType` 是 string，非 enum ✅

### 12.4 商业边界

- [ ] 开源 core 可独立编译、CLI 独立使用
- [ ] `credential.Resolver` 在开源 core 中
- [ ] Gateway 通过标准 OpenAI-compatible provider 支持，无商业代码
- [ ] AgentKit 是独立 Swift package
- [ ] Agent Gateway Service 是独立商业服务
