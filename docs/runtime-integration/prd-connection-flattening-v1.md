# PRD — Provider / Model / Credential 扁平化重构（跨 4 端）

> **状态**: 待评审
> **版本**: v1.0
> **日期**: 2026-08-03
> **类型**: 重大架构升级（跨 4 代码库协调）
> **设计依据**: `design-connection-flattening.md`（主文档 §0-13）、`design-connection-injection-channel.md`、`design-runtime-models-wire-v2.md`
> **影响调研**: `impact-tui-flattening.md`、`impact-agentkit-flattening.md`、`impact-chater-flattening.md`
> **涉及端**:
> 1. **code-agent runtime** — `/Users/xiaoyuan/Documents/work/git/code-agent`（Go，契约发布方）
> 2. **code-agent TUI/REPL** — `cmd/codeagent/`（Go，runtime 同仓消费者）
> 3. **AgentKit SDK** — `/Users/xiaoyuan/Documents/work/git/AgentKit`（Swift + gomobile）
> 4. **chater 宿主 app** — `/Users/xiaoyuan/Documents/work/git/chater`（Swift，iOS/macOS）
> 5. （外围，不改核心）**agent-gateway** — `/Users/xiaoyuan/Documents/work/git/agent-gateway`（Go，wire 消费端）

---

## 1. 背景与动机

### 1.1 现状问题（用户可感知）

1. **config.yaml 绑定工作区**：`cmd/codeagent/main.go:48` / `cmd/codeagentd/main.go:60` 硬编码 `LoadConfig("config.yaml")`，纯 cwd 相对加载。换目录启动缺 config.yaml 时只剩内置 deepseek，无法切换其他模型。
2. **模型/凭证配置归属混乱**：同一 ModelConfig 承载 base_url / context_window / pricing / credential，分散在三端各自维护，模型固有数据（pricing、context_window）被手抄且漂移。
3. **凭证体系三层嵌套**：`APIKey`(string) + `CredentialRef{namespace,name}` + Resolver 链（ChainResolver 可嵌套 5 层），`EnvResolver` 有 Mapping==nil 的隐式行为切换。

### 1.2 设计已定案的核心决策

| 决策 | 文档出处 |
|---|---|
| 凭证空间压扁为扁平 connection id，两级解析（会话覆盖 + connection 单一来源），无 Resolver 链 | flattening §4/§6 |
| 配置分层：内建 registry → `~/.codeagent/config.yaml` → `<cwd>/config.yaml` → 运行时注入 | flattening §8.3 |
| 连接定义归属按部署模式切换（AgentKit 场景 connectionsJSON 是唯一事实源） | flattening §8.4.1 |
| subagent_model 用户级化（codex 同）；permissions/verify_command 已归 P11 settings | flattening §8.5 |
| MCP 保持独立，仅共享凭证查询 | flattening §11 |
| 跨端桥接期（secretsJSON 三形态双读、catalog schema 前缀匹配、alias 迁移） | flattening §8.7 |
| connectionsJSON 通道独立于 secretsJSON（定义/值分离） | injection-channel §3 |
| `/v1/runtime/models` v2：两层结构保留、available 真实化、新增字段 optional | wire-v2 §4/§8 |

---

## 2. 范围

### 2.1 范围内

- 4 端的 provider/model/credential 扁平化改造（见 §4 分端规格）
- 跨端契约：connectionsJSON、secretsJSON 三形态、catalog v2
- 桥接期兼容与迁移

### 2.2 范围外（不在此次）

- `/login` + auth.json 文件（开放问题 Q3，后续）
- model catalog 生成化（开放问题 Q4，Go 静态 registry 先行，生成化后续）
- provider extension 机制（§8.6 决策做，但属独立工作项，可与本 PRD 并行规划）
- MCP 生命周期改造（MCP 保持独立）
- agent-gateway 服务端逻辑（只验证其 wire 消费端兼容）

---

## 3. 目标与非目标

### 3.1 目标

1. 任何目录启动 TUI/CLI 都有完整模型列表，不依赖 config.yaml 拷贝
2. 模型/凭证配置用户级化（桌面文件 / iOS 宿主注入），与 cwd 解耦
3. 凭证解析两级化，消灭 Resolver 链嵌套
4. AgentKit/chater 注入「意图」而非完整 ModelConfig
5. 四端在桥接期内平滑迁移，旧数据（Keychain、session、config.yaml）不丢失

### 3.2 非目标

- 不改变 agent-gateway 的认证语义（JWT 仍是宿主 OAuth 托管）
- 不引入新的模型能力数据源（生成化后置）
- 不统一 MCP 生命周期到 Connection

---

## 4. 分端需求规格

### 4.1 code-agent runtime（契约发布方，最先动）

**位置**: `internal/app`、`internal/credential`、`internal/runtime`、`internal/model`、`internal/server`、`internal/embed`

#### R1 凭证核心重构（§10 步 1-3）
- [x] R1.1 删除 `ModelConfig.APIKey`（派生 secret，`config.go:201`），凭证统一走 Credential（F3.1）——**`APIKeyEnv` 保留**为 env 声明字段（registry 填充、credentials 段引用、Resolver 在调用时读 env，不再 load 时快照）
- [ ] R1.2 扁平 Target：`Target{Namespace,Name}` → 扁平 id，保留 namespace 兼容解析（F1.1）
- [ ] R1.3 两级凭证解析：会话覆盖 + connection 单一来源，`effectiveCredentialResolver`/`BuildProvider` 内建链移除（`serve_builder.go:190-203`、`provider.go:22-51`）
- [ ] R1.4 `EnvResolver` 双重人格消除：映射显式声明或统一 `TOUPPER(id)_API_KEY` 约定
- [ ] R1.5 `api_key_env` 兼容期读取 + 警告（F3.3）

**验收**: `go test ./internal/...` 通过；无 `APIKey` 引用残留（`grep APIKey internal/` 仅剩迁移注释）

#### R2 配置分层 + registry（§10 步 4-5）
- [ ] R2.1 内建 registry：deepseek/qwen/glm/ollama/gateway 的 base_url + env 约定 + context_window + pricing（F2.1）
- [ ] R2.2 `LoadConfig` 分层加载：内建 → `~/.codeagent/config.yaml` → `<cwd>/config.yaml` → 注入（F2.5）；`LoadConfigBytes` 保留给嵌入
- [ ] R2.3 friendly-name → profile 映射（registry 保留旧名别名，`--model deepseek` 持续可解析）
- [ ] R2.4 分层合并规则落地（§8.3 定案：connection 覆盖/追加、凭证继承、default_model 三级优先级、能力数据优先）
- [ ] R2.5 `subagent_model` 用户级解析（严格 + fail-open 契约保留，`subagent.go:229-258`）
- [ ] R2.6 传输层参数迁移：registry 提供每 connection 默认 timeout/retry，`ProviderConfig` 全局 section 降为兜底（§2.4/F2.6）——`BuildProvider(conn, timeout)` 的 timeout 来源：registry 默认 → connectionsJSON 覆盖 → 全局兜底

**验收**: 删除 config.yaml 后 `./codeagent` 启动仍有完整模型列表；换目录不丢配置

#### R3 wire v2 + 注入契约（§10 步 6-7，对外契约）
- [ ] R3.1 `/v1/runtime/models` v2：available 真实化 + `unavailable_reason` + per-connection credential status/source；新增字段 optional；schema 前缀匹配 v1/v2（wire-v2 §8）
- [ ] R3.2 `Reconfigure(connectionsJSON, secretsJSON, modelName)` 三参（injection-channel §6）
- [ ] R3.3 secretsJSON 三形态 key 双读：扁平 / `{namespace}/{name}` / 遗留 env 名（flattening §8.7 项 1）
- [ ] R3.4 connectionsJSON 解析 → connection 定义（层级 4，AgentKit 场景唯一事实源）；支持 timeout 覆盖（registry 默认 → connectionsJSON 覆盖，§2.4）
- [ ] R3.5 `Options.ConnectionsJSON`（`embed/server.go` Options 扩展）

**验收**: 三形态 key 单元测试；v1/v2 schema 双输出测试；`Reconfigure` 三参空串保活

### 4.2 code-agent TUI/REPL（runtime 同仓，依赖 R1/R2）

**位置**: `cmd/codeagent/`（main.go、repl.go、tui/）、`internal/runtime/flags.go`

#### T1 模型选择兼容（调研建议 1-2）
- [ ] T1.1 `Config.SelectModel(friendlyName)` 保持稳定 API，内部走合并 profile 空间
- [ ] T1.2 `--model` / `/use` / `subagent_model` 经 friendly-name 映射继续解析（registry 旧别名）
- [ ] T1.3 `/use` picker 列表来自合并后的模型空间（`tui/model.go:860-908`、`run.go:200-203`）
- [ ] T1.4 决策：`--model` 是否额外接受 `connection/wire_model` 语法（open item）

#### T2 goal/subagent
- [ ] T2.1 `subagent_model` 用户级：严格解析 + fail-open（`goal.go:112-146`），不可解析时输出 degraded 警告
- [ ] T2.2 `printCostReport`（`main.go:322-341`）基于合并视图

**验收**: `go test ./cmd/codeagent/...` 通过；`--model deepseek`、`/use deepseek`、旧 session resume 均可解析

### 4.3 AgentKit SDK（依赖 R3 契约，可与 R1/R2 并行）

**位置**: `Sources/AgentKit/Core/`

#### A1 DTO 与 schema（调研阻碍 3-4）
- [ ] A1.1 `RuntimeServerModelCatalog` 等 DTO 新增字段 optional 化（`unavailable_reason`、credential status/source）
- [ ] A1.2 schema 硬校验改前缀匹配：`RuntimeServerCoordinator`（~451）/`RuntimeServerPreflight`（~230）接受 v1+v2
- [ ] A1.3 `UnifiedModelDescriptor`/`UnifiedModelCatalogStore` 保持宿主面抽象，alias 格式不变

#### A2 secretsJSON 双 key（调研阻碍 2、5）
- [ ] A2.1 `CredentialTarget.id` 持久化身份兼容：Keychain/UserDefaults 旧 `llm/<name>` 条目可读
- [ ] A2.2 `CredentialMap.toSecretsJSON()` 支持扁平 + namespaced 双写（桥接期）
- [ ] A2.3 `AgentSettings` 遗留 env-key 路径处理定案（迁移到 CredentialMap 或删除）

#### A3 connectionsJSON 序列化（injection-channel §4）
- [ ] A3.1 首等 Swift 类型 `ConnectionsJSON`/`RuntimeConnectionDefinition`（宿主不手拼 wire）
- [ ] A3.2 `RuntimeProviderConfigurationBuilder` 从 configYAML 迁移到 connectionsJSON 序列化

#### A4 gomobile ABI（调研阻碍 1）
- [ ] A4.1 `MobileStart`/`reconfigure` 加 connectionsJSON 参数；`"" = keep current` 语义
- [ ] A4.2 `AgentRuntime` 的 `reconfigure(connectionsJSON:secretsJSON:modelName:)` + 持久化 `(connectionsJSON, secretsJSON)` 对，`restart()` 存活
- [ ] A4.3 版本 bump（对标 `cb63a55 chore: bump CodeAgentRuntime to 1.3.2`），所有宿主需 rebuild

**验收**: `swift test` 通过（CredentialMapTests 更新 + connectionsJSON 测试）；旧 DTO 解码不因 v2 字段失败

### 4.4 chater 宿主（最末端，依赖 A4）

**位置**: `Talkify/Providers/`、`Talkify/AppContainer.swift`、`Talkify/Agent/`

#### C1 连接注入（调研建议 1-2）
- [ ] C1.1 `ProviderConnection.id` → connection id 1:1 序列化到 connectionsJSON（已是唯一小写 slug）
- [ ] C1.2 Gateway 特例连接（`.gatewayAccount`/`modelSource: .gatewayRemote`）走同一通道
- [ ] C1.3 `CompositeCredentialStore` 路由保留 `gateway`/`llm` 命名空间语义（`ProviderConnectionStore.swift:78-84`）

#### C2 迁移（调研阻碍 2、5）
- [ ] C2.1 Keychain `llm/<name>` → 扁平 id 一次性迁移（扩展 `migrateLegacyProviderState`，`AppContainer.swift:708-751`）
- [ ] C2.2 `CredentialTarget(namespace:name:)` 旧编码双读（`CredentialSettingsStore.swift:35-36`）
- [ ] C2.3 **AgentKit A2.3 删除决策的宿主侧配合**：AgentKit 已删除 `AgentSettings.secretsJSON()` 遗留通道，`CredentialSettings.migrateFromLegacyIfNeeded()` 存在但当前无调用点——chater 启动路径必须显式调用它，否则只写 `AgentSettings.apiKey` 的旧 Keychain 值不会迁入 CredentialMap，宿主会收到 `"{}"` 无注入

#### C3 catalog 消费（调研阻碍 4）
- [ ] C3.1 `publishAppliedCatalog` 门控 + revision 队列对接 v2 catalog（防未知 alias 污染 Composer）
- [ ] C3.2 `unifiedModelGroups` connectionID 分组在 connection+profile 两级模型下存活

#### C4 auth_expired（调研阻碍 6、建议 5）
- [ ] C4.1 `onAuthExpired`（`AppContainer.swift:284-292`）重注入走新 Reconfigure 三参通道
- [ ] C4.2 确保 OAuth 刷新后 Runtime 恢复 provider 调用前注入新鲜扁平 key

**验收**: iOS 模拟器手工流程：添加服务商 → 模型可选 → gateway JWT 过期 → 自动刷新恢复；Keychain 迁移后旧 session 可用

---

## 5. 跨端契约（冻结依据）

### 5.1 connectionsJSON（injection-channel §4）
```json
{ "connections": { "<connection_id>": {
  "api": "openai" | "ollama",
  "base_url": "https://...",
  "credential": { "source": "jwt" | "keychain" | "env" | "none", "ref": "...", "env": "..." }
} } }
```
> 注：无 `kind` 字段——MCP 不并入 Connection（flattening §11），connectionsJSON 中的连接**隐式都是 model connection**；MCP 走自己的 `.mcp.json`。

### 5.2 secretsJSON 三形态（flattening §8.7 项 1）
| 形态 | 示例 | 桥接处理 |
|---|---|---|
| 扁平 connection id | `deepseek`、`gateway` | 目标格式 |
| `{namespace}/{name}` | `llm/deepseek`、`gateway/default` | 映射（`llm/<x>`→`<x>`，`gateway/default`→`gateway`） |
| 遗留 env 名 | `DEEPSEEK_API_KEY` | 按 registry env 约定反查 |

### 5.3 catalog v2（wire-v2 §4）
- 保留 v1 字段，新增 `unavailable_reason`、`connection.credential.status/source`（**optional**）
- schema 前缀匹配 `runtime-model-catalog/v1|v2`
- alias 格式 `provider.<b64>.model.<b64>` 不变

### 5.4 Reconfigure
`Reconfigure(connectionsJSON, secretsJSON, modelName)` — 三参均可 `""` 表示保持当前

---

## 6. 依赖图与执行顺序

```
runtime R1/R2（凭证+分层） ──→ TUI T1/T2
        │
runtime R3（wire v2+注入） ──→ AgentKit A1-A4 ──→ chater C1-C4
        └────────────────────────────────────────┘
         （AgentKit 可按契约文档与 R3 并行开发，后联调）
```

### 波次

| 波次 | 内容 | 并行性 | 前置 |
|---|---|---|---|
| **波次 1** | runtime R1+R2（凭证核心 + 配置分层） | 串行（内部顺序） | 无（契约文档已冻结） |
| **波次 1'** | runtime R3（wire v2 + 注入契约） | 可与 R1/R2 并行（不同包面） | 无 |
| **波次 2** | AgentKit A1-A4 | 与波次 1 并行（按契约文档） | 契约冻结（已完成） |
| **波次 2'** | TUI T1-T2 | 与 AgentKit 并行 | R1/R2 落地 |
| **波次 3** | chater C1-C4 | 串行（最后） | AgentKit A4（gomobile ABI） |

### 关键路径
`R1/R2 → T1/T2` 与 `R3 → A1-A4 → C1-C4` 两条路径并行；**chater 是唯一串行末端**。

---

## 7. 里程碑

| 里程碑 | 内容 | 完成标准 | 联调点 | 状态 |
|---|---|---|---|---|
| **M1** | R1+R2 完成 | 换目录启动不丢模型；`go test ./internal/...` 绿 | TUI 可开工 | ✅ 达成（2026-08-04） |
| **M2** | R3 完成 | 三形态 key + v1/v2 schema 测试绿 | AgentKit 联调开始 | ✅ 达成（2026-08-04） |
| **M3** | A1-A4 完成 | `swift test` 绿；gomobile ABI bump | chater 开工 | ✅ 达成（2026-08-04） |
| **M4** | C1-C4 完成 | iOS 模拟器手工流程通过 | 四端集成验收 | ⚠️ 降级达成（见 §10 Q6：chater 既有链接缺陷阻塞测试执行） |
| **M5** | 集成验收 | 跨端用例全绿（§8） | 发布 | ✅ 達成（1.4.0 gomobile ABI 落地，AgentKit live 透传完成） |

---

## 8. 集成验收用例（跨端）

1. **换目录不丢配置**：TUI 在无 config.yaml 目录启动 → 完整模型列表（registry + 用户全局）
2. **BYOK 添加**：chater 添加 DeepSeek 连接 → connectionsJSON 注入 → Runtime catalog 出现 → 模型可用
3. **Gateway JWT 过期**：Runtime 发 `auth_expired` → chater OAuth 刷新 → 新 Reconfigure 三参重注入 → 恢复，无 Resolver 链参与
4. **旧 Keychain 迁移**：chater 升级后旧 `llm/<name>` 条目 → 扁平 id → 旧会话模型可用
5. **旧 config.yaml 兼容**：含 `api_key_env`/`credentials` 的旧配置读取 + 警告，不报错
6. **旧 SDK/新 runtime**：schema v1 前缀匹配，v1 客户端继续工作（available 恒 true 语义）
7. **gateway 消费 v2 catalog**：agent-gateway 读取 `/v1/runtime/models` v2 不报错（v2 字段 optional 时旧消费逻辑兼容）——外围组件验证，非改造

---

## 9. 风险与缓解

| 风险 | 等级 | 缓解 |
|---|---|---|
| gomobile ABI 变更波及所有宿主 rebuild | 高 | 版本 bump + 协调发布；`"" = keep current` 降破坏性 |
| AgentKit schema 硬校验阻塞 embedded 上下文加载 | 高 | 前缀匹配 v1/v2 先落地；v2 字段 optional |
| `CredentialTarget.id` 持久化身份变更 | 中 | 双 key 双读桥接期；Keychain 一次性迁移 |
| friendly-name→profile 映射遗漏 | 中 | registry 保留旧名别名；`SelectModel` API 稳定 |
| chater 是串行末端，延误会放大 | 中 | 前置准备（DTO 预研、迁移分析）在波次 2 并行做 |
| `--model`/`/use` 语义变化 | 低 | 保持 friendly-name 解析为主，`connection/wire` 语法为可选项 |
| gomobile ABI breakage 需回退 runtime 变更 | 中 | runtime 保留旧注入路径（ConfigYAML + secretsJSON 双读）兼容到 M4 gate——波次 2 联调若暴露 ABI 问题，runtime 可回退 R3 而波次 1（R1/R2 纯内部重构）不受影响；**波次 1/1' 的改动不依赖 gomobile ABI 变更**（R1/R2 不碰 embed 契约，R3 解析层与 ABI 解耦） |

---

## 10. 开放问题追踪

| # | 问题 | 状态 | 决策点 |
|---|---|---|---|
| Q1 | `/v1/runtime/models` wire 协议（输出结构对齐） | **已定案** | wire-v2 文档 §3/§4：保留两层结构、available 真实化、credential 子对象（status/source）、unavailable_reason；§8 兼容性前缀匹配 |
| Q2 | secretsJSON 桥接键冲突（扁平 id vs 旧友好名） | **已定案** | 三形态双读落地（R3.3）：`{namespace}/{name}`、扁平 id、遗留 env 名（明文直接注入 bearer） |
| Q3 | `/login` + auth 文件时机 | 后置 | 本 PRD 范围外 |
| Q5 | MCP 凭证命名冲突 | **已定案** | MCP 保持独立（§11），不并入 connection；扁平 id 冲突由 `flatID` 注释记录（llm/foo 与 mcp/foo 同名时后者覆盖），调用方避免 |
| Q6 | **chater 既有链接缺陷（非 flattening 引入）** | open（独立技术债） | AppContainer.swift:106,212 直接用 `ToolRegistry`，但只 `import ClientToolsKit`（ClientToolProtocol 是传递依赖，无 `@_exported`）——iOS Simulator 链接失败（ToolRegistry symbol 未进 app 链接输入），macOS 动态链接宽松通过。chater pbxproj 无任何 XCRemoteSwiftPackageReference（零直接远程依赖）。修复 = app target 显式声明 ClientToolProtocol 远程依赖（项目第一个直接远程引用）或重构 AppContainer 不直接依赖该传递类型。**阻塞 ProviderFlatteningTests 执行，导致 M4 降级** |
| AgentSettings 遗留 env-key 路径 | **已裁决** | A2.3 决策=删除 `AgentSettings.secretsJSON()`；宿主须显式调 `CredentialSettings.migrateFromLegacyIfNeeded()`（chater C2.3 已落地 AppContainer.swift:230） |
| `--model` 是否接受 `connection/wire_model` 语法 | **已裁决** | T1.4 决策=不扩；friendly-name 解析为主（registry 兜底覆盖已知连接） |

---

## 11. 执行清单（防遗忘）

### 启动前
- [ ] 确认 4 仓库 git 状态干净（避免并行改动撞车）
- [ ] 本文档 + 3 份设计文档 + 3 份 impact 报告归档为基线
- [ ] **go 工具链**：`go` 不在默认 PATH（`/opt/homebrew/bin/go`），所有验证命令用 `zsh -l -c` 包装；开发 session 的 build/test 一律 `zsh -l -c 'go build/test ...'`
- [ ] **基线测试 triage**（HEAD `4273bd7` 既有失败，非本次引入）：
  - `internal/runtime` — `TestServeRunBuilderRuntimeAliasStrictness`（serve_builder_test.go:138 "bare wire model must not fall back through a Direct Provider"）——**恰在 R1-R3 改造面**（resolveTurnModel），必须先弄清基线语义再动手
  - `internal/conversation` — `TestAcceptCrossSessionMessagePersistsProvenanceAndReturnsCursor`（executor_test.go:141 "unexpected wire model"）——跨 session 控制平面，与扁平化无关，可暂缓

### 波次 1（runtime）
- [ ] R1.1-R1.5 凭证重构 → M1 gate
- [ ] R2.1-R2.5 分层 + registry
- [ ] R3.1-R3.5 wire v2 + 注入 → M2 gate

### 波次 2（并行）
- [ ] AgentKit A1.1-A4.3 → M3 gate
- [ ] TUI T1.1-T2.2

### 波次 3（chater）
- [ ] C1.1-C4.2 → M4 gate

### 收尾
- [ ] 跨端集成用例（§8 七项）→ M5 gate
- [ ] 各仓库独立 commit（不跨仓库合 commit）
- [ ] 文档收尾：更新 flattening 文档状态为「已实现」，归档开放问题决策
