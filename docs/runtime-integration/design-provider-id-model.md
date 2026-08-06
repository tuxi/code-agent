# Provider 服务商 id 模型（对齐 pi）

> **状态**: 设计定案（实施中）
> **受众**: Runtime 开发 + AgentKit 开发
> **版本**: v1.0
> **日期**: 2026-08-05
> **前置**: design-connection-flattening.md、design-config-settings-merge.md

---

## 1. 问题

当前 `models.<name>.provider` 字段语义是 **api 类型**（`openai` / `responses` / `ollama`）——用户必须自己知道每个服务商走什么协议、手写 base_url。这让 settings.json 里的服务商配置难以理解：

```json
"models": {
  "deepseek": {
    "provider": "openai",                                   // ← api 类型，不是服务商
    "base_url": "https://api.deepseek.com",                 // ← 手写 URL
    "model": "deepseek-v4-flash",
    "credential": {"namespace": "llm", "name": "deepseek"}
  }
}
```

用户的心智是「我要用 DeepSeek / OpenRouter / 阿里」，不是「我要用 openai 协议连某个 URL」。

## 2. 目标模型（对齐 pi）

**`provider` 字段语义改为「服务商 id」**。已知服务商零配置（registry 补 base_url + api 类型 + env 约定），未知的走通用兜底：

```json
"models": {
  "deepseek":   { "provider": "deepseek",   "model": "deepseek-v4-flash" },
  "openrouter": { "provider": "openrouter", "model": "deepseek/deepseek-chat" },
  "dashscope":  { "provider": "openai",
                  "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
                  "model": "qwen-plus" }
}
```

- **已知服务商**（`deepseek` / `qwen` / `glm` / `openrouter` / `ollama` / `gateway`）：只写 `provider: <id>` + `model`，base_url / api 类型 / env 约定由 registry 推导
- **未知服务商**：写 `provider: <api类型>`（`openai` = chat-completions 兼容 / `responses` / `ollama`）+ 显式 `base_url` —— 通用兜底
- **api 类型由 registry 推导**：服务商 id → `ProviderType`（`openai` / `responses` / `ollama`），`BuildProvider` 分派不变

### 服务商 id 命名空间（不撞 api 类型）

| 服务商 id | api 类型 | base_url | env | 备注 |
|---|---|---|---|---|
| `deepseek` | openai | `https://api.deepseek.com` | `DEEPSEEK_API_KEY` | BYOK |
| `qwen` | openai | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `DASHSCOPE_API_KEY` | 阿里 DashScope |
| `glm` | openai | `https://open.bigmodel.cn/api/paas/v4` | `GLM_API_KEY` | 智谱 |
| `openrouter` | openai | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` | **新增** |
| `ollama` | ollama | `http://localhost:11434/v1` | — | 本地 |
| `gateway` | openai | 注入 | — | 宿主注入 |

**不设 `openai` 服务商 id**——它与 api 类型 `openai` 撞名。OpenAI 官方走通用兜底：`provider: responses` + `base_url: https://api.openai.com/v1`（pi 的 openai 也用 responses）。api 类型全集保持现状（openai/responses/ollama），服务商 id 与之正交。

---

## 3. 对 gateway / 鉴权的影响（分析结论：零影响）

### 3.1 鉴权完全不受影响

鉴权路径由 **`Credential` ref → `credential.Resolver`** 决定，与 `provider` 字段**解耦**：

```
models.<name>.credential: {namespace, name}
        │
        ▼
credential.Resolver.Resolve(target)  ← gateway/default → JWT；llm/<name> → env key；injected
        │
        ▼
Provider.applyAuth(credential)       ← 与 provider 服务商 id 无关
```

- **gateway**：`provider: gateway` + `credential: {gateway, default}` —— registry 补 `ProviderType: openai`（wire 协议），JWT 走 Credential.Target，**完全不变**
- **BYOK**：`provider: deepseek` + `credential: {llm, deepseek}` —— env 约定由 registry 填，解析不变
- **injected（嵌入式）**：`provider: gateway` + credential source injected —— secretsJSON → resolver，不变

### 3.2 BuildProvider 分派不变

`BuildProvider` 的 `switch mc.Provider`（openai/ollama/responses）**不改**——服务商 id 在 `normalizeConfig` 阶段解析为 api 类型填回 `mc.Provider`。BuildProvider 看到的是解析后的 api 类型。

### 3.3 `/v1/runtime/models` 的 ProviderID 更准确

现状 `ProviderID = Catalog.ProviderID ?? mc.Provider`。改动后 `mc.Provider` 是 api 类型（解析后），服务商 id 落到 `Catalog.ProviderID` —— `provider_id` 字段显示服务商身份（deepseek/openrouter），语义更准确。

### 3.4 兼容约束（必须保留）

- **显式值不覆盖**：`applyRegistryDefaults` 只填空字段——存量 `provider: openai + base_url: https://api.deepseek.com` 完全不变
- **SelectModel 回退**（config.go:602）硬编码 `Provider: "openai"` 保留——未声明模型默认走 chat-completions（避免隐式切换协议）
- **gateway 的 `"gateway": {}`** 保留——base_url/env/wire_model 由宿主注入，registry 不填

---

## 4. 实施

### 4.1 registry.go

```go
type builtinConnection struct {
    BaseURL      string
    Env          string
    WireModel    string
    ProviderType string // 新增："openai" | "responses" | "ollama"
}

var builtinConnections = map[string]builtinConnection{
    "deepseek":   {BaseURL: "https://api.deepseek.com", Env: "DEEPSEEK_API_KEY", WireModel: "deepseek-v4-flash", ProviderType: "openai"},
    "qwen":       {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Env: "DASHSCOPE_API_KEY", WireModel: "qwen3-coder-plus", ProviderType: "openai"},
    "glm":        {BaseURL: "https://open.bigmodel.cn/api/paas/v4", Env: "GLM_API_KEY", WireModel: "glm-4.7", ProviderType: "openai"},
    "openrouter": {BaseURL: "https://openrouter.ai/api/v1", Env: "OPENROUTER_API_KEY", ProviderType: "openai"},  // 新增
    "ollama":     {BaseURL: "http://localhost:11434/v1", ProviderType: "ollama"},
    "gateway":    {ProviderType: "openai"},  // base_url/env/wire_model 由宿主注入
}
```

### 4.2 applyRegistryDefaults（服务商 id → api 类型）

```go
func applyRegistryDefaults(mc *ModelConfig) {
    conn, ok := builtinConnections[mc.Name]  // 按 friendly name 匹配服务商 id
    if !ok {
        return
    }
    if mc.BaseURL == "" {
        mc.BaseURL = conn.BaseURL
    }
    if mc.APIKeyEnv == "" {
        mc.APIKeyEnv = conn.Env
    }
    // 服务商 id → api 类型：仅当用户写的是服务商 id（mc.Provider == mc.Name
    // 或为空）时解析；显式 api 类型（openai/responses/ollama）不覆盖。
    if conn.ProviderType != "" && (mc.Provider == "" || mc.Provider == mc.Name) {
        mc.Provider = conn.ProviderType
    }
    // 服务商 id 落到 Catalog.ProviderID 供 /v1/runtime/models 展示。
    if mc.Catalog.ProviderID == "" {
        mc.Catalog.ProviderID = mc.Name
    }
}
```

**关键判断**：`mc.Provider == mc.Name` 时（用户写 `provider: deepseek` 且模型名也叫 deepseek）解析；`mc.Provider == "openai"` 且模型名 deepseek 时（存量配置）——不解析，保留 api 类型。

### 4.3 测试

- 服务商 id 解析：`provider: deepseek` → `mc.Provider == "openai"` + `Catalog.ProviderID == "deepseek"` + base_url/env 填充
- 存量兼容：`provider: openai + base_url` → 不覆盖，保持原样
- 新增 openrouter 服务商：base_url/env/api 类型正确
- gateway：`provider: gateway` → ProviderType openai + base_url 空（注入）

---

## 5. 验收

- 用户配 DeepSeek 只写 `provider: deepseek` + `model`，零手写 URL
- 用户配 OpenRouter 只写 `provider: openrouter` + `model`（model 名可带 `/`）
- 未知服务商走 `provider: openai/responses` + base_url 通用兜底
- gateway / 鉴权路径完全不变（Credential ref → resolver）
- 存量 settings.json 兼容（显式值不覆盖）
