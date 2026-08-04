# Code-Agent Provider / Model / Credential 统一设计（Connection 扁平化）

> **状态**: 设计探索阶段，未进入实现
> **受众**: Runtime 开发 + AgentKit 开发 + 架构评审
> **版本**: v0.1 — 首版（对标 pi 的扁平模型调研）
> **日期**: 2026-08-03

---

## 0. 背景与问题

### 0.1 三套并行机制在同时运作

当前系统存在三套并行的凭证/Provider 构建路径，之间存在隐式桥接和 fallback：

```
        旧路径                    新路径                 运行时路径
   ┌──────────────┐    ┌──────────────────┐    ┌─────────────────────┐
   │ api_key_env   │    │ credentials:      │    │ ctx.Credential      │
   │ (环境变量)     │    │   llm:            │    │ (per-turn session   │
   │               │    │     deepseek:     │    │  credential from    │
   │               │    │       source: env │    │  WS handshake)      │
   └──────┬───────┘    └────────┬─────────┘    └──────────┬──────────┘
          ▼                     ▼                         ▼
   mc.APIKey (string)    mc.Credential (ref)     ctx.Credential (Resolver)
          │                     │                         │
          └────── 桥接 ────────┘                         │
          LoadConfigBytes 自动推导                        │
          CredentialRef from APIKey                      │
                    │                                    ▼
                    ▼                           effectiveCredentialResolver(session, base)
           BuildProvider(mc, pc, cred)
                    │
                    ├─ cred==nil → EnvResolver
                    ├─ cred!=nil → ChainResolver
                    │    [cred, EnvResolver]
                    ▼
           OpenAICompatibleProvider
              ├─ .Credential (Resolver)
              ├─ .APIKey     (string fallback)
              └─ applyAuth: Credential → APIKey
```

### 0.2 具体绕点

1. **双轨凭证**：`ModelConfig` 同时持有 `APIKey`/`APIKeyEnv`（旧式）与 `Credential`（新式引用）两个凭证形态（`internal/app/config.go:163-196`）。`LoadConfigBytes` 会自动桥接：配了 `api_key_env` 但没写 `credential` 时，推导出 `CredentialRef{Namespace:"llm", Name:<model name>}`（`internal/app/config.go:466-471`）。同一个 API key 以「具体字符串」和「凭证指针」两种形态同时存在。

2. **BuildProvider 重复造 Resolver 链**：`Config.CredentialResolver(injected)` 已构建完整 Resolver 链（injected → configured env → default env），但 `BuildProvider` 无视它，自己重新组合 `cred + EnvResolver`（`internal/runtime/provider.go:30-37`）——等于在一条链外面再包一层链。

3. **Provider 内部三层 fallback**：`applyAuth` 先走 `Credential.Resolve()`，失败再落 `APIKey` 静态 key（`internal/model/openai_compatible.go`）。认证逻辑分散在三处。

4. **effectiveCredentialResolver 又一层链**：每个 turn 的 session credential 与 base credential 合并（`internal/runtime/serve_builder.go:190-203`），合并结果又被 `BuildProvider` 再包一层。最终有效链可达 5 层嵌套。

5. **EnvResolver 双重人格**：`Mapping==nil` 时走硬编码 `defaultEnvMapping`（仅 llm namespace，`TOUPPER(name)_API_KEY`）；`Mapping!=nil` 时走配置显式映射（`internal/credential/env.go:10-41`）。行为切换完全隐式，且 `CredentialResolver` 里造的 `EnvResolver` 与 `BuildProvider` 里裸造的 `EnvResolver` 行为不同、类型相同。

### 0.3 三端的本质差异

| 模式 | 凭证模型 | 模型选择 | 配置 owner |
|------|---------|---------|-----------|
| TUI/CLI | 每服务商一个 API key（env var） | 用户 config.yaml 声明 | 用户 |
| AgentKit | 每服务商一个 API key / 一个 gateway JWT（注入） | 宿主注入 | 宿主 app |
| agent-gateway | 一个 JWT → 一个端点 → 服务端选模型 | 服务端（`model: ""`） | 宿主认证系统 |

三个模式、三种配置来源，全塞在同一个 `ModelConfig` 结构体里——这是「绕」的根源。

---

## 1. 参考：pi 的扁平模型

pi（`../pi`）是 TypeScript monorepo，其 provider/模型/凭证管理是三层各自闭环的扁平模型：

### 1.1 Provider = 编译期内建

38 个 provider 全部是编译期已知的 slug（`deepseek`、`anthropic`、`openai`…），每个在 `packages/ai/src/providers/<name>.ts` 一个实现文件，懒注册。base URL **内建在 provider 实现里**，用户不需要知道 `api.deepseek.com`。选模型就是 `provider/modelId` 一个字符串。用户自定义端点才在 `~/.pi/agent/models.json` 里出现 `baseUrl`（`packages/coding-agent/docs/custom-provider.md`）。

### 1.2 模型 = 生成的数据

`models.generated.ts` 由 `scripts/generate-models.ts` 从上游数据源抓取生成，CI 有 `publish-model-catalog` 发布流程。AGENTS.md 明文规定：**不手改 `models.generated.ts`，改 generator 重新生成**。每个模型带全套能力元数据：

```typescript
interface Model {
  id, name, api, provider, baseUrl
  reasoning: boolean
  thinkingLevelMap       // thinking 等级 → provider 参数
  input: ("text" | "image")[]
  cost: ModelCost         // 定价
  contextWindow, maxTokens
}
```

### 1.3 凭证 = 一个 provider 一个凭证，扁平无命名空间

三种来源全部按 **provider slug 索引**：

- **环境变量约定**：`<PROVIDER>_API_KEY`，`env-api-keys.ts` 一张硬编码映射表（`DEEPSEEK_API_KEY` → `deepseek`）
- **auth.json**：`~/.pi/agent/auth.json`，type-tagged（`api_key` / `oauth`），由 `/login` 交互式写入、`/logout` 清除
- **models.json 内联**：自定义 provider 的 `apiKey` 支持 `!command`（1Password）、`$ENV` 插值、字面量

核心抽象 `CredentialStore` 四个方法：`read / list / modify / delete`，`modify` 是唯一写路径（序列化 read-modify-write，OAuth refresh 跨进程防并发）。每个 provider 定义自己的 `ApiKeyAuth.resolve()` / `OAuthAuth`——env、AWS profile、ADC、OAuth 各自的解析逻辑在各自 provider 里，**没有一个通用的 Resolver 链**（`packages/ai/src/auth/types.ts`）。

### 1.4 pi 哲学

> **provider 是代码，模型是数据，凭证是文件。**

code-agent 把三者都做成了配置，且配置分散在三端。

---

## 2. 核心概念：Connection（概念归一）

### 2.1 目标形态

不管哪种模式，Runtime 看到的永远是同一种东西：

```
Connection（连接层 — 怎么到达 API）：
  ID         string   // 扁平、全局唯一（原来 "name"）
  Kind       string   // "model" | "mcp"（原来是 namespace）
  API        string   // "openai" | "ollama"（原来 provider type）
  BaseURL    string   // 内建 registry 提供，自定义才配
  Credential CredentialSource  // 单一凭证声明

ModelProfile（模型层 — 怎么用这个模型）：
  ConnectionRef string   // 指向哪个 Connection
  WireModel     string   // "deepseek-v4-pro" / ""（gateway 服务端决定）
  Temperature, ContextWindow, Pricing
  DisplayName, SupportsTools, InputModalities  // 展示元数据
```

### 2.2 薄 Connection + 独立 ModelProfile

- **一个 Connection 可服务多个 ModelProfile**：gateway 场景一个 JWT 连接背后 N 个模型，每个有自己的 context_window / pricing。
- **凭证刷新不影响模型配置**：JWT 过期换 token 只更新 Connection；切换 temperature 只更新 ModelProfile。
- **展示元数据的 owner 不同**：`connection_id`/`display_name` 是 AgentKit 宿主注入的；`context_window`/pricing 是模型固有属性。

### 2.3 凭证的活值语义

`auth_expired` 机制（`docs/runtime-integration/credential-injection-v1.md:242-276`）意味着凭证不是一次性解析的静态值——Gateway JWT 会过期，宿主收到事件后调 `Reconfigure` 注入新 token。因此 Connection 的凭证是「每次请求时解析」的，不是「构建时填入」的字符串。

### 2.4 传输层参数（ProviderConfig）归属

`ProviderConfig`（`internal/app/config.go:156-161`：request_timeout_seconds / max_retries / backoff_millis / max_backoff_seconds）是全局 section，经 `BuildProvider(mc, pc, cred)` 进入 `ResilientProvider`。归属规则：

- **默认值来自内建 registry**——每个 connection 有各自的传输层默认
- **connection 定义里可覆盖**——自定义端点可调大超时
- **gateway 超时 > 直连超时**——prefill 时间不同
- `ProviderConfig` 全局 section 保留为「兜底默认」，不再由 `BuildProvider` 逐次传入（§10 第 4 步的 `BuildProvider(conn, timeout)`）

---

## 3. 概念映射表（pi → code-agent）

| pi | code-agent 现状 | code-agent 目标 | 对应 pi 的实现 |
|---|---|---|---|
| provider slug（`deepseek`） | `llm/deepseek`（namespace+name） | 扁平 connection id `deepseek` | `KnownProvider` 联合类型 |
| gateway = 一个 provider（`radius`） | `gateway/default` + `model: ""` | 内建 connection `gateway`（动态 catalog） | pi 的 `radius` provider |
| MCP 服务器 | `mcp/github` namespace | connection `github`（kind: mcp） | —（pi 无 MCP，扩展） |
| base_url 内建在 provider 里 | 每个 model 手写 `base_url` | 内建 registry，只有自定义端点才配 | provider 实现文件 |
| `Model`（id/contextWindow/cost/input/reasoning） | 手写 `models:` section + `catalog:` 元数据 | 生成/内建 catalog，配置只覆盖 | `models.generated.ts` + generator |
| 凭证 key = provider id | `CredentialRef{Namespace, Name}` | 凭证直接挂在 connection 上 | auth.json 按 provider id 索引 |
| `<PROVIDER>_API_KEY` 约定 | EnvResolver llm namespace 默认映射 | 推广到所有连接：`TOUPPER(id)_API_KEY` | `env-api-keys.ts` envMap |
| 每 provider 一个 `resolve()` | 通用 ChainResolver 层层嵌套 | connection 声明单一来源 + 会话覆盖两级 | `ApiKeyAuth.resolve()` / `OAuthAuth` |
| auth.json 文件存储 | env + credentials section + 注入 | （可选）`/login` + auth 文件 | `CredentialStore` |
| `/login` 交互式登录 | 无 | TUI 增加 `/login` | `/login` |

---

## 4. 核心变换：三维 → 一维

```
现在：Target{Namespace, Name}
  llm/deepseek     gateway/default      mcp/github
    │                  │                    │
    ▼                  ▼                    ▼
目标：Connection{ID, Kind, ...}
  {ID: deepseek, Kind: model}   {ID: gateway, Kind: model, DynamicCatalog: true}
  mcp → 不并入 Connection（§11）
```

- **namespace → kind 字段**（llm/gateway 变成 connection 的 type；mcp 仅共享凭证解析，不并入 Connection 模型）
- **name → id**（扁平、全局唯一）
- `CredentialRef{Namespace, Name}` → `Connection.Credential`，直接写在连接上，不再有独立引用

关键事实：`EnvResolver` 在 llm namespace 下的默认规则（`TOUPPER(name)_API_KEY`，`internal/credential/env.go:12-18`）**就是 pi 的 envMap 约定**。扁平化只是把这条规则从 llm 推广到所有 connection。

---

## 5. 三端映射

### 5.1 TUI/CLI 模式

**现状**（`config.example.yaml`）：

```yaml
credentials:
  gateway:
    default: {source: injected}
  llm:
    deepseek: {source: env, env: DEEPSEEK_API_KEY}

models:
  deepseek:
    provider: openai
    base_url: "https://api.deepseek.com"     # 用户要记住 URL
    model: "deepseek-v4-flash"
    credential: {namespace: llm, name: deepseek}   # 独立引用层
    context_window: 128000                   # 手抄数据
    input_price_per_million: 0.27
```

**目标**（扁平）：

```yaml
default_model: deepseek/deepseek-v4-pro
# ↑ base_url、凭证约定、context_window、pricing、capabilities
#   全部来自内建 catalog

connections:
  my-proxy:
    base_url: "https://proxy.example.com/v1"   # 自定义端点才出现 URL
    credential: env:MY_PROXY_KEY

models:
  deepseek-flash:
    connection: my-proxy
    wire_model: deepseek-v4-flash
    temperature: 0.2
```

消失：`credentials:` section、每个 model 的 `credential:` ref、手写 `base_url`、手抄 `context_window`/pricing。首次使用只需 `export DEEPSEEK_API_KEY=...` + 选模型名。

### 5.2 AgentKit 嵌入模式

**现状**：宿主注入 `Options{ConfigYAML, ModelName, Secrets}`（`internal/embed/server.go:61-119`），Secrets 键是新格式 target（`gateway%2Fdefault`）或旧格式 env 名/模型友好名。

**目标**：宿主的「连接列表」设置页直接序列化注入，Runtime 不重新解释：

```json
{
  "connections": {
    "gateway":  {"api": "openai", "baseUrl": "https://agent.xxx.com", "credential": {"source": "jwt"}},
    "deepseek": {"api": "openai", "credential": {"source": "keychain", "ref": "deepseek.key"}},
    "ollama":   {"api": "openai", "baseUrl": "http://localhost:11434/v1", "credential": {"source": "none"}}
  }
}
```

Secrets 扁平化（`injectSecrets`/`parseTargetKey`，`internal/embed/server.go:937+`）：

```
旧: {"gateway%2Fdefault": {"type":"bearer","secret":"eyJ..."}}
新: {"gateway":   {"type":"bearer","secret":"eyJ..."}}
旧: {"DEEPSEEK_API_KEY": "sk-..."} 或 {"llm%2Fdeepseek": ...}
新: {"deepseek": {"type":"bearer","secret":"sk-..."}}
```

桥接期两种都接受：`llm%2Fdeepseek` → connection `deepseek`，`gateway%2Fdefault` → connection `gateway`。**宿主是配置 owner 的定位不变**——变的是注入形态从「完整 config 副本」变成「扁平连接列表」。

### 5.3 agent-gateway 模式

**现状**：`model: ""` sentinel + `credentials.gateway.default: {source: injected}`。

**目标**：gateway 是内建 connection，模型目录由服务端动态下发并缓存：

```yaml
connections:
  gateway:
    credential: injected          # JWT，文件里没有任何 secret
    # base_url 内建，可覆盖

models:
  gateway-default:
    connection: gateway
    wire_model: ""                # 服务端选择（保留现有语义）
```

pi 的 `radius` 已验证此模式：`/login radius` 存 OAuth token，gateway catalog 独立刷新、缓存在 `models-store.json`。code-agent 的 gateway 只是把 OAuth 换成宿主注入的 JWT。

---

## 6. 凭证模型统一（扁平 + 两级）

目标凭证解析只有两级，不再有链：

```
Resolver(connectionID):
  1. 会话覆盖层   ← per-turn WS 凭证（对应 pi 的 --api-key overlay）
  2. connection 声明的单一来源：
     - injected（宿主 secrets，按 connection id 索引）
     - env:<VAR>（默认约定 TOUPPER(id)_API_KEY，即 pi 的 envMap）
     - auth 文件（未来 /login，pi 的 auth.json）
     - none（Ollama 本地）
```

直接消除的绕点：

- **EnvResolver 双重人格消失**：映射要么显式声明、要么走统一约定，不再有 `Mapping==nil` 的隐式切换
- **BuildProvider 不再包链**：传入的 resolver 已是最终形态，去掉内部 `EnvResolver`/`ChainResolver` 重建（`internal/runtime/provider.go:22-51`）
- **`ModelConfig.APIKey`/`APIKeyEnv` 删除**：三个凭证字段收敛成 connection 上的一个 `Credential`
- **MCP 共享凭证解析而非模型**：MCP 服务器走同一两级凭证查询（`design-mcp-credential-resolution.md`），但连接定义/生命周期/工具图留在 MCP 自己的体系（§11），不并入 Connection 模型

---

## 7. 数据归属

| 数据 | owner | 存放位置 |
|---|---|---|
| connection 定义（base_url、凭证约定、内建 api） | code-agent 仓库（内建 registry） | 三层分层：内建 registry + 用户全局 + 项目覆盖（§8） |
| model catalog（context_window、pricing、capabilities） | code-agent 仓库（生成数据）+ gateway 服务端（动态） | 内建 catalog + 宿主展示元数据注入 |
| 凭证值 | 用户（TUI）/ 宿主（AgentKit）/ 宿主认证系统（gateway JWT） | env / Keychain → secretsJSON / auth 文件 |
| 选哪个模型 | 用户 / 宿主 | `default_model` / `Options.ModelName` |

**Runtime 永远是被注入的一方，不自己决定用什么 provider。** 注入形态从「完整 ModelConfig 副本」升级为「扁平 connection + catalog 覆盖 + 按 id 索引的凭证」。

---

## 8. 配置分层（用户级 vs 项目级）

### 8.1 现状问题

主配置是**纯 cwd 相对加载**：`cmd/codeagent/main.go:48` 与 `cmd/codeagentd/main.go:60` 硬编码 `app.LoadConfig("config.yaml")`，缺文件就落回内置默认（只有 deepseek，`internal/app/config.go:422-430`）。没有 home 目录查找、没有向上搜索、没有层级合并——换目录启动就得把 config.yaml 拷到工作区，否则只剩默认模型。

`~/.codeagent` 确实存在，但只放 DataDir（session 数据库）、`mcp.json`、`skills/`、`prompts/`、P11 的 `settings.json`（`cmd/codeagent/main.go:53-72`）。**主配置（models/credentials）从不从那里加载**——`~/.codeagent` 的「兼职」状态正是「config.yaml 奇怪」的根源。

### 8.2 pi 的对照

pi 把模型和凭证配置放在**全局用户目录**，任何目录生效（`packages/coding-agent/src/config.ts:515-566`）：

```
全局 ~/.pi/agent/：  models.json（用户模型）、auth.json（凭证）、settings.json
项目 <cwd>/.pi/：    settings.json、extensions/、prompts/、skills/、themes/
```

models.json 和 auth.json **只存在于全局层**（项目级资源列表里没有它们，`resource-loader.ts:800-803`）。组合是三层：built-in → models.json → extensions（`provider-composer.ts:411-438`，modelOverrides 为最顶层用户层）。任何目录 `--list-models` 都有完整模型列表。

### 8.3 目标分层

```
层级 1: 内建 registry（代码内建）
     — base_url、env 约定（TOUPPER(id)_API_KEY）、context_window、pricing、capabilities
     — 任何目录开箱即用（对标 pi 的 models.generated.ts + env-api-keys.ts）

层级 2: ~/.codeagent/config.yaml（用户全局，新增）
     — 覆盖/追加 connection 与模型，一次写好任何目录生效
     — subagent_model 等用户偏好（§8.5）
     — 凭证约定（env:<VAR>）与未来 auth 文件（~/.codeagent/auth.json）

层级 3: <cwd>/config.yaml（项目级）
     — 只留选择语义（default_model 等）；模型定义迁出（§8.5）
     — 兼容期保留 models 读取 + 警告，随后删除（§8.6）
     — permissions/verify_command 已由 P11 迁入 settings 体系，config.yaml 仅低优先 back-compat

层级 4: 运行时注入（AgentKit connectionsJSON + secrets / gateway JWT）
     — AgentKit 场景下 connectionsJSON 是连接定义的**唯一事实源**（§8.4.1）
     — secretsJSON 按 connection id 索引，覆盖以上静态层
     — TUI 场景下无此层（或仅 gateway token）
```

**与 P11 settings 体系的关系**：P11（已 shipped，`docs/p11-project-settings.md`）已把项目行为（`permissions`/`verify_command`/`hooks`）从 config.yaml 迁入四层 settings——`~/.codeagent/settings.json`（用户级）+ `.codeagent/settings.json`（项目共享）+ `.codeagent/settings.local.json`（项目本地）+ config.yaml 低优先 back-compat。config 分层只承担 **infrastructure**（模型/连接/凭证/偏好），**behavior 归 settings 体系**——两者互补、不重叠。加载机制**复用 P11 的同一 loader/优先级模式**（`internal/approve/rules.go` 已实现 Scope 分层），不另起炉灶。

组合规则：**后一层覆盖前一层**（内建 < 用户全局 < 项目 < 注入）。模型/凭证配置与 cwd 解耦——任何目录启动都有完整模型列表，不再需要拷贝 config.yaml。

**具体合并规则**（定案，替代开放问题 6 的悬空）：

- **connection**：同 id 跨层出现 → 后层整条覆盖该 connection（含 base_url/credential）；不同 id → 追加。层级 4 注入的 connection 覆盖所有静态层
- **model profile**：同 (connection, wire_model) 跨层出现 → 后层覆盖该 profile；新组合 → 追加
- **凭证**：connection 覆盖时若后层未声明凭证 → 继承前层凭证声明（不因覆盖而丢）；层级 4 注入的凭证值覆盖静态层声明
- **default_model**：项目层设了 → 项目级优先；未设 → 用户全局层；再未设 → 内建 registry 默认
- **subagent_model**：用户级（§8.5）——项目层一般不放；未设 → 继承主模型
- **catalog 元数据**：能力数据（context_window/pricing）归 registry，展示元数据（display_name/supports_tools）归 host 注入——冲突时**能力数据优先**（能力决定行为，展示只影响 UI，§12 开放问题 9）

对应 pi 的分层：

| | pi | code-agent |
|---|---|---|
| 基础能力 | 内建 catalog（models.generated.ts） | 内建 registry |
| 用户覆盖 | `~/.pi/agent/models.json` | `~/.codeagent/config.yaml`（新增） |
| 项目覆盖 | `<cwd>/.pi/settings.json` + 资源 | `<cwd>/config.yaml`（现有） |
| 扩展层 | extensions（`provider-composer`） | provider extension 机制（决策：做，§8.6） |
| 运行时覆盖 | `--api-key` overlay | 会话覆盖 + 宿主注入 |

### 8.4 对三端的影响

- **TUI/CLI**：换目录不再丢模型/凭证——主配置读全局层，项目层只放增量
- **AgentKit**：宿主注入本来就是层级 4，语义不变；config.yaml 的加载路径改动不影响注入协议。**连接定义归属按部署模式切换（§8.4.1 定案）**——AgentKit 场景下，宿主注入的 connectionsJSON 是连接定义的**唯一事实源**（层级 4），registry 只提供能力默认值（context_window/pricing/capabilities），host 设置页完全保留控制权；TUI 场景下，用户级 `~/.codeagent/config.yaml` 是事实源。两套事实源按部署模式切换，不在同一进程内共存竞争
- **Gateway**：内建 registry 提供 gateway connection，JWT 走层级 4，天然匹配

### 8.4.1 连接定义归属（三端调研定案）

三端调研（`impact-tui-flattening.md` / `impact-chater-flattening.md` / `impact-agentkit-flattening.md`）验证了「配置归属」的真实约束：**chater 宿主完全拥有连接配置**（`ProviderConnection` 存 UserDefaults、`ProviderConnection.id` 已是唯一小写 slug、`publishAppliedCatalog` 门控防未知 alias），iOS 沙箱没有 `~/.codeagent`。

定案：

| 部署模式 | 连接定义事实源 | registry 角色 |
|---|---|---|
| TUI/CLI | 用户级 `~/.codeagent/config.yaml`（层级 2） | 能力默认值兜底 |
| AgentKit（embedded） | 宿主注入的 connectionsJSON（层级 4，唯一事实源） | 能力默认值兜底 |
| agent-gateway | 内建 gateway connection + 服务端动态 catalog | 提供 gateway 定义 |

- **修正**：设计文档此前「连接定义放进 runtime 拥有的层（内建 registry + `~/.codeagent/config.yaml`）」对 AgentKit 场景不成立——host 是且继续是连接配置的唯一 owner。connectionsJSON 通道（`design-connection-injection-channel.md`）承载的就是宿主事实源。
- **registry 不竞争**：AgentKit 场景下 registry 不注入连接定义，只提供能力元数据（context_window/pricing/capabilities）供 host 展示元数据覆盖；两者不冲突。
- **推论**：TUI 场景的层级 2 用户全局层与 AgentKit 场景的 connectionsJSON 是**同一概念的两种载体**——用户级配置在桌面上以文件形式存在，在 iOS 上以宿主注入形式存在，语义对齐。

### 8.5 定义 vs 选择：config.yaml 能删什么

`models:` section 混着两类职责，删除能力取决于区分：

| 职责 | 内容 | 能否从 config.yaml 删 |
|---|---|---|
| **定义**（怎么到达、怎么用） | base_url、credential、context_window、pricing、capabilities | 能——迁到内建 registry + 用户全局层 |
| **选择**（这个项目用哪个） | `default_model` | 不能——项目语义，「团队统一主模型」能力 |
| **用户偏好**（子代理用哪个） | `subagent_model` | 用户级——性能/成本偏好，与项目无关（codex 亦在 `~/.codex`） |

pi 的证明：项目层 `.pi/` 没有模型定义（`resource-loader.ts:800-803` 项目级资源只有 skills/prompts/themes/extensions），模型 = 内建 catalog + 用户全局 models.json。

删除后的目标形态：

```yaml
# <cwd>/config.yaml — 只留项目语义
default_model: deepseek/deepseek-v4-pro   # 选择，不是定义
mcp: {...}
# permissions/verify_command 已由 P11 迁入 .codeagent/settings.json（§8.3）
```

项目级边界：**只留「选择 + mcp」**（permissions/verify_command 已归 P11 settings 体系，§8.3）。

| 归属 | config.yaml 内容 | 去向 |
|---|---|---|
| 用户级 | models（定义）、connections、credentials | `~/.codeagent/config.yaml` |
| 用户级/部署级 | server（监听/display_name） | `~/.codeagent/config.yaml` 或部署环境 |
| 用户级（偏好） | `subagent_model` | `~/.codeagent/config.yaml`；缺省继承主模型（codex 同） |
| 项目级（选择） | `default_model` | 留在 `<cwd>/config.yaml`；缺省用用户默认 |
| 项目级（集成） | mcp | `<cwd>/config.yaml` + `.mcp.json` |
| 项目级/用户级（行为） | `permissions`、`verify_command`、`hooks` | 已在 P11 settings 体系（`~/.codeagent/settings.json` + `.codeagent/settings.json` + `.codeagent/settings.local.json`），config.yaml 仅低优先 back-compat |

项目层缺省时完全不干扰：任何目录启动默认可用（内建 registry 开箱 + 用户全局层一次写好的自定义），只有项目真正需要差异化（团队统一主模型、MCP）才放项目级——安全边界与构建命令由 P11 settings 体系按 Scope 分层承载。

删除的前提（缺一不可）：

1. **内建 registry 覆盖要够**——至少覆盖 config.example.yaml 的用例（deepseek/qwen/glm/ollama/gateway），base_url、env 约定、context_window、pricing 全内建。否则删 models 后用户只剩内置一个模型，体验比现在更差。
2. **用户全局层 `~/.codeagent/config.yaml` 先建好**——自定义端点（公司代理、自建模型）的定义迁到这一层。
3. **项目级自定义 provider 有承接机制**——见 §8.6。

### 8.6 项目级自定义 provider：extension 机制（决策：做）

**问题**：code-agent 没有 pi 的 extensions 机制——`internal/plugins` 是 skill marketplace（`.claude-plugin/marketplace.json`，`internal/plugins/plugins.go:1-8`），不是 provider 注册机制。项目级独有端点（公司内部 gateway、团队自建服务）在删掉 config.yaml 的 models 后没有承接处。

**选项**：

- (a) 项目层保留最小 `connections:` section（只放定义，不放模型能力）——改动小，但与「config.yaml 无模型」的目标相悖
- (b) **provider extension 机制（对标 pi，工作量大）——决策采用**

**对标 pi**：项目级扩展走 `<cwd>/.pi/extensions/`（`loader.ts:694-695`），`provider-composer.ts:411-438` 把 built-in → models.json → extensions 三层组合，extensions 可注册自定义 provider、覆盖模型、注入凭证。code-agent 的等价物：注册点 + 组合层 + 生命周期（加载/热更新）。

**删除 models 的三阶段时序**：

```
阶段 1（flattening，§10 前 7 步）: 建 registry、建全局层、config.yaml 引入 connections
阶段 2（deprecate）:               models 降级为「兼容读取 + 警告」，新项目不再写；
                                   extension 机制就位（承接项目级自定义 provider）
阶段 3（delete）:                  读取逻辑删除，config.yaml 彻底无模型定义
```

**删除时机**：registry 覆盖面是最大不确定项。**registry + 全局层 + extension 机制证明能兜住 config.example.yaml 全部用例之后，才进入阶段 2**。阶段 3 不设硬性时间点，以迁移完成度为依据。

### 8.7 跨端桥接期（三端调研定案）

三端调研（`impact-tui-flattening.md` / `impact-chater-flattening.md` / `impact-agentkit-flattening.md`）确认：**桥接期是刚性需求，不是可选项**——三端各自有持久化身份或硬校验，无法一次性切换。桥接期契约：

**1. secretsJSON 双 key 读取**（AgentKit + chater）

运行时在桥接期**同时接受**三种 key 形态：

| 形态 | 示例 | 处理 |
|---|---|---|
| 扁平 connection id | `deepseek`、`gateway` | 目标格式 |
| `{namespace}/{name}` | `llm/deepseek`、`gateway/default` | 映射到扁平 connection id（`llm/<x>` → `<x>`，`gateway/default` → `gateway`） |
| 遗留 env 名 | `DEEPSEEK_API_KEY` | 桥接期映射到 connection（按 registry env 约定反查）；AgentKit `AgentSettings` 遗留路径的处理需定案（迁移到 CredentialMap 或删除） |

宿主侧约束（来自调研）：AgentKit 的 `CredentialTarget.id` 是**持久化身份**（Keychain 账户、UserDefaults 存储），chater 的 Keychain 仍存 `llm/<name>` 条目——两端都要能**双写/双读**，直到宿主完成一次性迁移（chater 已有 `migrateLegacyProviderState` 先例）。

**2. catalog schema 前缀匹配**（AgentKit）

AgentKit `RuntimeServerCoordinator` / `RuntimeServerPreflight` 硬校验 `schema == "runtime-model-catalog/v1"`。v2 发布时：

- 运行时**同时输出 v1 与 v2 语义**（v2 字段 optional、增字段不改名），或 schema 前缀匹配 `runtime-model-catalog/v1|v2`
- AgentKit 桥接期接受 v1+v2，`unavailable_reason`/credential status 为 optional
- 旧 SDK 二进制对新 runtime：v1 字段仍可用（`available` 恒 true 语义）；新 SDK 对旧 runtime：缺 v2 字段走 optional

**3. alias / 模型引用迁移**（TUI + chater）

- TUI：已持久化 session 的 friendly-name alias 需映射到扁平 alias（`resolveTurnModel` 已有 bare-string fallback 先例，`serve_builder.go:176-187`）
- chater：`publishAppliedCatalog` 门控「只发布 Runtime 已确认的 catalog」必须在桥接期保留，防未知 alias 污染 Composer
- registry 保留旧 friendly name 作为别名（wire-v2 §6 option a），让 `--model deepseek`、`/use deepseek`、`subagent_model: deepseek` 持续可解析

**4. 桥接期时长与退出条件**

- 时长：持续到宿主侧 Keychain 条目完成一次性迁移 + 全量 SDK 客户端升级
- 退出：运行时停止接受旧 key 形态 + schema v1；以迁移完成度为依据，不设硬性时间点（同 §8.6 阶段 3）

---

## 9. Gateway 与 OAuth 的关系澄清

agent-gateway 不是「Runtime 走 OAuth」，而是「**托管 OAuth —— token 生命周期在宿主，Runtime 只当消费者**」：

```
宿主认证系统 ──OAuth/OIDC──→ 宿主应用 ──注入──→ Runtime ──Bearer 转发──→ Gateway
   ▲                          │                    │
   └──── refresh 在这里 ──────┘                    └─ 只探测 401，不碰 token
```

- **宿主认证系统 vs 宿主应用**：是 OAuth。`auth_expired` 流程里 `AccountManager` 持有 refresh token，调 `POST /api/v1/auth/refresh` 换新 access_token，存 Keychain。
- **宿主 vs Runtime**：不是。Runtime 不跑任何 OAuth 流程——没有 authorize/token 端点、没有 refresh、不知道 refresh token 存在。拿到的就是一个 opaque Bearer token，通过 secretsJSON 注入、原样转发。
- **Runtime vs Gateway**：纯 Bearer 转发。唯一「智能」是探测 401 → 发 `auth_expired` 事件 → 宿主刷新 → `Reconfigure` 注入新 token。

与 pi 的差别：pi 的 `radius` 是**真 OAuth in-runtime**（`OAuthAuth.refresh()` 在 `Models.modify()` 锁内执行，runtime 自己持有并刷新 refresh token）。code-agent 把整个 OAuth 客户端推到宿主侧——这是嵌入场景（iOS AccountManager、Keychain、系统级认证）下的正确取舍，也正解释了「凭证归属哪一端」的纠结：**token 语义在宿主（OAuth），token 消费在 Runtime（Bearer），中间靠注入协议桥接**。

推论：Runtime 不需要知道 gateway 凭证是 OAuth 还是 JWT 还是 API key，只需要 `connection_id → bearer secret` 一件事。OAuth 复杂性应完全留在宿主侧，不进 Runtime 的 Resolver 链。

---

## 10. 迁移顺序（风险从低到高）

1. **删 `ModelConfig.APIKey`/`APIKeyEnv`**（纯内部，`internal/app/config.go`）— 凭证统一走 Credential
2. **扁平 Target**：`Target{ID}`，保留 namespace 兼容解析（`internal/credential/types.go`）— secretsJSON 桥接在 `parseTargetKey`
3. **config 引入 `connections:` section**：凭证从 `credentials:` 移入 connection（`LoadConfigBytes`）
4. **内建 registry**：已知 connection 的 base_url/env 约定内建，`BuildProvider` 简化为 `BuildProvider(conn, timeout)`（`internal/runtime/provider.go`）
5. **config 分层加载**：`LoadConfig` 从 cwd 相对改为「内建 → `~/.codeagent/config.yaml` → `<cwd>/config.yaml` → 注入」分层合并（§8）；`~/.codeagent` 从 DataDir 兼职升级为主配置家目录
6. **model catalog 生成化**：context_window/pricing/capabilities 进内建数据，`/v1/runtime/models` 输出统一
7. **chain 消除**：`CredentialResolver` 两级化，去掉 `effectiveCredentialResolver` 与 `BuildProvider` 的双层包裹（`internal/runtime/serve_builder.go:190-203`）
8. **provider extension 机制**：项目/用户级自定义 provider 注册（对标 pi 的 extensions，§8.6）；就绪后 config.yaml 的 `models:` 按 §8.6 三阶段（deprecate → delete）移除

---

## 11. 不扁平化的边界

- **per-turn session 凭证**：不是链，是覆盖层（pi 的 `--api-key` overlay 对应物，scope 是会话）
- **宿主展示元数据注入**：`ModelCatalogMetadata`（`connection_id`/`provider_id`/`display_name`，`internal/app/config.go:198-208`）保留，AgentKit UI 需要
- **gateway 动态 catalog**：服务端是模型目录 owner，Runtime 缓存刷新（radius 模式）
- **MCP 生命周期独立**：MCP 与 Connection 只共享凭证解析（两级查询）。连接定义、每工作区工具图（`WSReg`/`ToolReg` 按 workspace 实例，每 turn `CheckReloadMCP`/`ReloadMCP` 热重载，`internal/runtime/serve_builder.go:293-305`）留在 MCP 自己的体系；`.mcp.json` 文件形态保留（Claude 兼容）

---

## 12. 开放问题

1. **/v1/runtime/models wire 协议**：**已定案**——`design-runtime-models-wire-v2.md` §3/§4：保留两层结构、available 真实化、credential 子对象（status/source）、unavailable_reason；§8 兼容性前缀匹配
2. **secretsJSON 桥接期键冲突**：`deepseek` 作为 connection id 与旧友好模型名匹配的歧义优先级
3. **`/login` + auth 文件**：要不要现在做（pi 的 auth.json 等价物），还是先只保留 env + injected
4. **内置 model registry 的数据源**：**默认决策：Go 静态内建先行**（几十个模型手写一份，验证完所有流程后再评估生成化）。生成化（上游数据源 + CI publish）是后续优化，不阻塞阶段 1（§8.6 gate 解除）
5. **MCP 凭证并入两级查询**：MCP 服务器不并入 Connection 空间（§11），但凭证查询按 connection id 索引——MCP server 的凭证 target 命名需避免与 connection id 冲突（如 mcp server 也叫 `github`）或加前缀
6. **分层合并语义**：**设计期定案**——具体规则见 §8.3（connection 覆盖、凭证继承、default_model 优先级、catalog 元数据优先级）
7. **host 注入 connection 定义的通道**：**已定案（§8.4.1）**——connectionsJSON 通道（`design-connection-injection-channel.md`）承载宿主连接定义，AgentKit 场景下是唯一事实源；选项采用「独立 connectionsJSON」，排除扩展 secretsJSON 与 extension 承载。F4.1/F4.2（AgentKit 迁移）据此排期
8. **runtime alias 稳定性**：session 持久化按 runtime alias 引用模型（`runtimeAliasComponents`，`internal/server/runtime_contract.go:106`，格式如 `provider.bWlzc2luZw.model.bWlzc2luZw`）。扁平化改变 alias 生成规则后，**已持久化 session 里保存的模型引用会失效**，需要 alias 映射或迁移
9. **catalog 元数据合并优先级**：context_window/supports_tools/input_modalities 等字段能力数据归 registry、展示元数据归 host 注入——冲突时谁赢需显式定义（§8.3 已定：能力数据优先）

---

## 13. 最终形态与需求清单

### 13.1 最终形态

**概念模型**：

```
Connection（连接层 — 怎么到达 API）
  ID, Kind（model | mcp）, API, BaseURL, Credential（单一声明）
ModelProfile（模型层 — 怎么用）
  ConnectionRef, WireModel, Temperature, ContextWindow, Pricing, 展示元数据
凭证解析（两级，无链）：
  1. 会话覆盖（per-turn WS）
  2. connection 声明的单一来源（injected / env:<VAR> / auth 文件 / none）
```

**配置分层**（后层覆盖前层）：

```
层级 1: 内建 registry（代码内建）      — base_url、env 约定、context_window、pricing、capabilities
层级 2: ~/.codeagent/config.yaml      — 模型定义、连接、凭证约定、全局 server 偏好
层级 3: <cwd>/config.yaml            — 只留 default_model + mcp；permissions/verify_command 已归 P11 settings（仅低优先 back-compat）
层级 4: 运行时注入                    — AgentKit connectionsJSON（唯一事实源）+ secrets，按 connection id 索引
```

**三端形态**：

| 端 | 最终形态 |
|---|---|
| TUI/CLI | 任何目录开箱可用；主配置读用户级，项目层只放增量；首次使用 = 设 env var + 选模型 |
| AgentKit | 注入「意图」而非完整配置；host 设置页经 connectionsJSON 注入 connection 定义（§8.4.1 已定案）；凭证走 secretsJSON 按 id 索引；桥接期双 key 读取（§8.7） |
| Gateway | 内建 connection + 服务端动态 catalog；JWT 走层级 4；Runtime 只做 Bearer 转发 + 401 探测 |

### 13.2 需求清单

**F1 概念统一**
- F1.1 凭证空间压扁为 connection id，`CredentialRef{Namespace,Name}` 移除（兼容解析保留）
- F1.2 Connection 凭证为活值（每次请求解析），支持 `auth_expired` 刷新
- F1.3 凭证解析只有两级（会话覆盖 + connection 单一来源），无 Resolver 链

**F2 配置分层**
- F2.1 内建 registry：已知连接（deepseek/qwen/glm/ollama/gateway）base_url + env 约定 + context_window + pricing 内建
- F2.2 `~/.codeagent/config.yaml`：用户级模型定义、连接、凭证约定、subagent_model 偏好；任何目录生效
- F2.3 `<cwd>/config.yaml`：只留选择（default_model）+ mcp；permissions/verify_command 已归 P11 settings 体系（§8.3）
- F2.4 分层合并规则明确（覆盖 vs 追加、default_model 优先级、凭证继承、catalog 元数据优先级）——具体规则已定案（§8.3）
- F2.5 任何目录启动默认可用，不再需要拷贝 config.yaml
- F2.6 传输层参数归属：ProviderConfig 默认值归 registry，connection 可覆盖，gateway 超时更长（§2.4）

**F3 删除与迁移**
- F3.1 `ModelConfig.APIKey`（派生 secret 字段）删除，凭证统一走 Credential；`APIKeyEnv` 保留为 env 声明（registry 填充、resolver 调用时读 env，不再 load 时快照）
- F3.2 `models:` section 三阶段移除（flattening → deprecate 警告 → delete）
- F3.3 现有 `api_key_env` 配置在兼容期读取 + 警告
- F3.4 runtime alias 迁移：已持久化 session 的模型引用映射到新 alias（开放问题 8）
- F3.5 跨端桥接期：secretsJSON 三形态 key 读取 + catalog schema v1/v2 前缀匹配 + registry 保留旧 friendly 别名（§8.7）

**F4 三端**
- F4.1 AgentKit：secretsJSON 键扁平化（`llm%2Fdeepseek` → `deepseek`），桥接期三种形态都接受（§8.7）
- F4.2 AgentKit：host 注入 connection 定义经 connectionsJSON 通道（§8.4.1 已定案）
- F4.3 Gateway：保留 `wire_model: ""` 服务端选择语义
- F4.4 TUI：`/v1/runtime/models` 目录与 runtime 行为同源（消除 pricing/context_window 漂移）；TUI 继续读 `Config` 而非 wire catalog（调研建议 7）

**F5 扩展**
- F5.1 provider extension 机制：项目/用户级自定义 provider 注册（注册点 + 组合层 + 加载/热更新）
- F5.2 MCP 服务器共享两级凭证查询；连接定义/生命周期/工具图留在 MCP 体系（§11）

**N1 非功能**
- N1.1 迁移全程向后兼容（旧 config.yaml、旧 secretsJSON 格式可用）
- N1.2 凭证值永不落盘、永不进入 `/v1/runtime/models` 输出
- N1.3 registry 默认 Go 静态先行（开放问题 4 已定案）；生成化作为后续优化，不阻塞阶段 1（§8.6 gate 解除）

### 13.3 验收信号

- [ ] 删除 config.yaml 后启动：仍有完整模型列表（内建 + 用户全局），默认模型可用
- [ ] 换目录启动：模型/凭证配置不变，无拷贝步骤
- [ ] 添加已知服务商：只传 `{connection, wire_model, display_name}`，不序列化能力数据
- [ ] gateway JWT 过期：Runtime 发 `auth_expired`，宿主刷新后 `Reconfigure` 恢复，无 Resolver 链参与
- [ ] 配置中不再出现 `api_key_env`、`credential: {namespace, name}`、手写 `base_url`（已知连接）
- [ ] 项目级 config.yaml 只含选择 + mcp（permissions/verify_command 走 P11 settings 体系）
- [ ] 已持久化 session 的模型引用可迁移（旧 alias → 新 alias），或给出明确迁移工具
