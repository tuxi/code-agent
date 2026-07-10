# Agent Gateway API Contract v1.1

> **角色：** 本文件是 Agent Gateway、AgentKit (Swift)、Code-Agent (Go) 三方之间的正式 API 契约。
>
> **原则：** 以本文档为准，不以任何一方的实现代码为准。任何偏离本文档的行为视为 bug。
>
> **v1.1 变更：** 新增 Token 刷新边界规则、Model 增加 category/recommended_for 字段、`unit_factor` → `billing_factor` 全局改名、新增 Agent Session/Execution Header。

---

## 目录

1. [通用约定](#1-通用约定)
2. [Authentication API](#2-authentication-api)
3. [Account & Profile API](#3-account--profile-api)
4. [Agent Chat API](#4-agent-chat-api)
5. [Model API](#5-model-api)
6. [Usage API](#6-usage-api)
7. [Client 角色分工](#7-client-角色分工)
8. [错误码参考](#8-错误码参考)

---

## 1. 通用约定

### 1.1 Base URL

```
生产环境: https://api.example.com
开发环境: http://localhost:12221
```

所有路径以 `/api/v1` 为前缀。

### 1.2 统一响应格式

```json
{
  "trace_id": "uuid",
  "code": 0,
  "msg": "success",
  "data": { }
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `trace_id` | string | 请求追踪 ID，与 Gateway 日志关联 |
| `code` | int | 业务错误码，`0` 表示成功 |
| `msg` | string | 人类可读消息 |
| `data` | object/null | 响应数据负载 |

### 1.3 认证方式

所有需要鉴权的接口在 HTTP Header 中携带：

```
Authorization: Bearer <access_token>
```

**Token 管理规则：**

| 规则 | 说明 |
|------|------|
| access_token 有效期 | 由登录响应 `access_exp` 指定（Unix 秒） |
| refresh_token 有效期 | 由登录响应 `refresh_exp` 指定（Unix 秒） |
| 刷新方式 | `POST /api/v1/auth/refresh` |
| 匿名用户 | 匿名注册返回的 token 与正式用户 token 结构相同 |

**Token 刷新边界（关键约束）：**

```
Runtime 层                    宿主层 (AgentKit/CLI)
──────────                    ─────────────────────
收到 HTTP 401                 │
  │                           │
  ├─ emit auth_expired event  │
  │                           │
  │                           ├─ 收到 auth_expired
  │                           ├─ POST /auth/refresh
  │                           ├─ 获取新 access_token
  │                           ├─ 存储到 Keychain
  │                           │
  │◄── reconfigure(token) ────┤
  │                           │
  ├─ 重试请求                  │
```

> **Runtime 不持有 refresh_token，不调用 `/auth/refresh`，不实现 token 刷新逻辑。**
>
> 当 Runtime 收到 HTTP 401 时，唯一行为是向上层 emit `auth_expired` 事件/error。Token 刷新的完整生命周期由宿主（AgentKit/CLI）管理。

### 1.4 客户端 Header

所有客户端请求**建议**携带以下 Header：

```
X-Device-ID: <device_identifier>
X-Device-Type: ios | macos | cli
X-App-Version: <semver>
```

**Agent Chat API 专用 Header：**

```
X-Agent-Session-ID: <session_uuid>     # 一次长期对话（跨多次 execution）
X-Execution-ID: <execution_uuid>       # 一次 agent run
```

> **用途：** tracing、billing 归因、debugging。Gateway **不保存** agent conversation 内容，这些 Header 仅用于用量关联和问题排查。

### 1.5 HTTP 状态码约定

| 状态码 | 含义 |
|--------|------|
| 200 | 成功（含业务错误，见 `code` 字段） |
| 400 | 请求参数错误 |
| 401 | Token 无效或过期 |
| 403 | 权限不足（如匿名用户访问需正式注册的接口） |
| 404 | 资源不存在 |
| 429 | 配额耗尽（Agent API 专用） |
| 500 | 服务器内部错误 |

---

## 2. Authentication API

> **调用方：** AgentKit (Swift)
>
> **不应调用方：** Code-Agent Runtime

### 2.1 密码登录

```
POST /api/v1/auth/login/password
```

**Request Body:**

```json
{
  "username": "string (required)",
  "password": "string (required)"
}
```

**Response `data` (AuthRes):**

```json
{
  "is_new": false,
  "access_token": "eyJhbGciOiJIUzI1NiIs...",
  "refresh_token": "rt_xxxxxxxxxxxx",
  "access_exp": 1750000000,
  "refresh_exp": 1752600000,
  "user_id": 10001,
  "role": 2,
  "account_type": "registered",
  "nickname": "Ivan"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `is_new` | bool | 是否为新注册用户 |
| `access_token` | string | JWT，Bearer 鉴权使用 |
| `refresh_token` | string | 用于刷新 access_token |
| `access_exp` | int64 | access_token 过期时间（Unix 秒） |
| `refresh_exp` | int64 | refresh_token 过期时间（Unix 秒） |
| `user_id` | int64 | 用户 ID |
| `role` | int | 角色码（1=Guest, 2=User, 5090=Admin） |
| `account_type` | string | `"anonymous"` 或 `"registered"` |
| `nickname` | string | 用户展示名 |

### 2.2 密码注册

```
POST /api/v1/auth/register/password
```

**Request Body:** 同 [2.1 密码登录](#21-密码登录)

**Response `data`:** 同 [AuthRes](#21-密码登录)（`is_new` 为 `true`）

### 2.3 手机验证码登录

```
POST /api/v1/auth/phone/login-by-code
```

**Request Body:**

```json
{
  "phone": "13800138000",
  "code": "123456"
}
```

**Response `data`:** 同 [AuthRes](#21-密码登录)

### 2.4 发送手机验证码

```
POST /api/v1/auth/phone/send-code
```

**Request Body:**

```json
{
  "phone": "13800138000"
}
```

**Response `data`:**

```json
{
  "success": true
}
```

### 2.5 阿里一键登录

```
POST /api/v1/auth/phone/login-by-one-tap
```

**Request Body:**

```json
{
  "access_token": "string (required - 阿里云号码认证 SDK 返回的 accessToken)",
  "out_id": "string (optional)"
}
```

**Response `data`:** 同 [AuthRes](#21-密码登录)

### 2.6 Apple 登录

```
POST /api/v1/auth/apple/login
```

**Request Body:**

```json
{
  "identity_token": "string (required - Apple IdentityToken JWT)",
  "authorization_code": "string (required - Apple 授权码)",
  "email": "string (optional)",
  "given_name": "string (optional)",
  "family_name": "string (optional)"
}
```

**Response `data`:** 同 [AuthRes](#21-密码登录)

### 2.7 匿名注册

```
POST /api/v1/anonymous/register
```

> 无需 Authorization Header。App 首次启动调用，满足 Apple 5.1.1(v) 未登录可订阅要求。每次调用生成新的 `user_id`。

**Response `data`:** 同 [AuthRes](#21-密码登录)（`account_type` 为 `"anonymous"`）

### 2.8 刷新 Token

```
POST /api/v1/auth/refresh
```

**Request Body:**

```json
{
  "refresh_token": "rt_xxxxxxxxxxxx"
}
```

**Response `data`:** 同 [AuthRes](#21-密码登录)（返回新的 access_token 和 refresh_token）

### 2.9 登出

```
POST /api/v1/auth/logout
Authorization: Bearer <access_token>
```

**Response `data`:** `{}`

> Token 被加入黑名单。客户端应清除本地存储的 token。

### 2.10 安全状态查询

```
GET /api/v1/auth/security-status
Authorization: Bearer <access_token>
```

**Response `data`:**

```json
{
  "user_id": 10001,
  "phone_masked": "138****8000",
  "has_phone": true,
  "has_apple": true,
  "has_password": false,
  "can_bind_phone": false,
  "can_bind_apple": false,
  "can_unbind_apple": true,
  "bound_login_methods": ["phone", "apple"],
  "recommended_login_methods": ["password"],
  "preferred_login_method": "phone"
}
```

### 2.11 绑定手机号

```
Step 1: POST /api/v1/auth/bind/phone/send-code
Authorization: Bearer <access_token>  (需 RequireRegistered)

Step 2: POST /api/v1/auth/bind/phone/confirm
Authorization: Bearer <access_token>  (需 RequireRegistered)
```

**Send Code Request:**
```json
{ "phone": "13800138000" }
```

**Confirm Request:**
```json
{ "phone": "13800138000", "code": "123456" }
```

**Response `data`:** `{ "success": true, "message": "" }`

### 2.12 绑定/解绑 Apple

```
POST /api/v1/auth/bind/apple
POST /api/v1/auth/unbind/apple
Authorization: Bearer <access_token>  (需 RequireRegistered)
```

**Bind Request:** 同 [Apple 登录请求](#26-apple-登录)
**Unbind Request:** 无 Body

**Response `data`:** `{ "success": true, "message": "" }`

---

## 3. Account & Profile API

> **调用方：** AgentKit (Swift)
>
> **不应调用方：** Code-Agent Runtime

### 3.1 获取用户资料

```
GET /api/v1/user/profile
Authorization: Bearer <access_token>
```

**Response `data`:**

```json
{
  "user_id": 10001,
  "username": "ivan",
  "nickname": "Ivan",
  "avatar_url": "https://cdn.example.com/avatars/10001.png",
  "phone_masked": "138****8000",
  "has_phone": true,
  "has_apple": true,
  "is_active": true,
  "register_source": "apple",
  "created_at": 1750000000,
  "updated_at": 1750000000,
  "subscription_active": true,
  "current_subscription": "dreamlog.sub.monthly",
  "subscription_expired_at": 1752600000,
  "available_points": 5000,
  "frozen_points": 0,
  "can_use_1080p": true,
  "can_use_hd_image": true,
  "can_remove_watermark": true,
  "can_use_priority_queue": false,
  "can_use_custom_aspect_ratio": false,
  "point_discount_rate": 0.9
}
```

### 3.2 更新用户资料

```
PATCH /api/v1/user/profile
Authorization: Bearer <access_token>  (需 RequireRegistered)
```

**Request Body:**

```json
{
  "nickname": "Ivan",
  "avatar_url": "https://cdn.example.com/avatars/10001.png"
}
```

> 所有字段 optional，只传需要更新的字段。

---

## 4. Agent Chat API

> **调用方：** Code-Agent Runtime (Go)
>
> **这是 Code-Agent 唯一需要调用的 Agent 接口。**
>
> **不应调用方：** AgentKit (Swift) — AgentKit 不直接发起 LLM 调用。

### 4.1 Chat Completion

```
POST /api/v1/agent/chat/completions
Authorization: Bearer <access_token>
Content-Type: application/json
```

**OpenAI 兼容格式。** Code-Agent 的 OpenAI Provider 将 `base_url` 设为 Gateway endpoint 即可，无需修改调用逻辑。

**Request Body:**

```json
{
  "model": "deepseek-v4-pro",
  "messages": [
    {
      "role": "system",
      "content": "You are a coding assistant..."
    },
    {
      "role": "user",
      "content": "分析 src/auth/ 目录的代码结构"
    },
    {
      "role": "assistant",
      "content": "我将分析该目录...",
      "tool_calls": [
        {
          "id": "call_xxx",
          "type": "function",
          "function": {
            "name": "read_file",
            "arguments": "{\"path\":\"src/auth/handler.go\"}"
          }
        }
      ]
    },
    {
      "role": "tool",
      "tool_call_id": "call_xxx",
      "content": "package auth\n\n// file content..."
    }
  ],
  "max_tokens": 4096,
  "temperature": 0.7,
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "read_file",
        "description": "Read a file from the filesystem",
        "parameters": {
          "type": "object",
          "properties": {
            "path": { "type": "string", "description": "文件路径" }
          },
          "required": ["path"]
        }
      }
    }
  ],
  "stream": true
}
```

| 字段 | 类型 | 必需 | 说明 |
|------|------|:---:|------|
| `model` | string | ✅ | 模型 ID，从 `GET /models` 获取。如 `"deepseek-v4-pro"` |
| `messages` | array | ✅ | 标准 OpenAI messages 格式 |
| `max_tokens` | int | | 最大输出 token 数，默认模型上限 |
| `temperature` | float | | 采样温度 0-2 |
| `tools` | array | | 工具定义数组 |
| `stream` | bool | | 是否 SSE 流式返回，默认 `false` |

### 4.2 非流式响应

```json
{
  "id": "chatcmpl-xxxxxxxx",
  "execution_id": "exec-xxxxx",
  "model": "deepseek-v4-pro",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "该目录包含以下文件...",
        "tool_calls": [
          {
            "id": "call_xxx",
            "type": "function",
            "function": {
              "name": "read_file",
              "arguments": "{\"path\":\"src/auth/service.go\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": {
    "prompt_tokens": 2500,
    "completion_tokens": 180,
    "total_tokens": 2680,
    "billing_units": 8040
  },
  "quota_remaining": {
    "daily_units": 491960,
    "weekly_units": 24196000,
    "monthly_units": 49919600
  }
}
```

### 4.3 流式响应 (SSE)

```
Content-Type: text/event-stream
```

每个 chunk 为标准 OpenAI 兼容的 SSE 格式：

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Let"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" me"},"finish_reason":null}]}

...

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2500,"completion_tokens":180,"total_tokens":2680}}

data: [DONE]
```

> **注意：** 当 `stream_options.include_usage` 开启时，最后一个非 `[DONE]` chunk 会携带 `usage` 字段。

### 4.4 配额耗尽响应

```json
HTTP 429 Too Many Requests

{
  "code": 42001,
  "msg": "daily limit exceeded. Used: 500,000 units, Limit: 500,000 units. Resets at 2026-07-10T00:00:00+08:00.",
  "data": {
    "limit_type": "daily",
    "units_used": 500000,
    "units_limit": 500000,
    "resets_at": "2026-07-10T00:00:00+08:00",
    "suggestion": "Switch to BYOK mode or wait for quota reset."
  }
}
```

### 4.5 Runtime 401 行为规范

```
Runtime 收到 401 Unauthorized
  │
  ├─ 不重试（避免重复消耗配额）
  ├─ 不调用 /auth/refresh（不属于 Runtime 职责）
  ├─ 向上层 emit auth_expired 事件
  │
  └─ 宿主（AgentKit/CLI）负责：
       ├─ POST /auth/refresh → 新 token
       ├─ 存储新 token
       └─ reconfigure Runtime
```

### 4.6 Gateway 对 Runtime 的透明性

**运行时只知道：**

- 一个 endpoint URL
- 一个 Bearer token
- 一个 session_id（可选，透传）
- 一个 execution_id（可选，透传）

**运行时不知道：**

- 后端是 DeepSeek 还是其他模型
- 用户的 API key
- 配额和计费逻辑
- Web search / tool 代理的实现
- JWT 内部结构
- Token 过期时间
- 如何刷新 Token

**这就是 Provider 抽象的边界。**

---

## 5. Model API

> **调用方：** Code-Agent Runtime + AgentKit
>
> **用途：** Runtime 获取可用模型列表做模型选择；AgentKit 展示模型信息。

### 5.1 获取模型列表

```
GET /api/v1/agent/models
Authorization: Bearer <access_token>
```

**Response `data`:**

```json
{
  "models": [
    {
      "id": "deepseek-v4-pro",
      "display_name": "DeepSeek V4 Pro",
      "provider": "deepseek",
      "capabilities": ["chat", "coding", "tool_call"],
      "context_window": 128000,
      "max_output_tokens": 8192,
      "supports_streaming": true,
      "supports_tool_calls": true,
      "supports_reasoning": false,
      "category": "reasoning",
      "recommended_for": ["coding_agent", "complex_refactor", "architecture"],
      "input_billing_factor": 1.00,
      "output_billing_factor": 3.00,
      "available": true
    },
    {
      "id": "deepseek-v4-flash",
      "display_name": "DeepSeek V4 Flash",
      "provider": "deepseek",
      "capabilities": ["chat", "coding", "tool_call"],
      "context_window": 128000,
      "max_output_tokens": 8192,
      "supports_streaming": true,
      "supports_tool_calls": true,
      "supports_reasoning": false,
      "category": "fast",
      "recommended_for": ["chat", "simple_edit", "routine_task"],
      "input_billing_factor": 0.27,
      "output_billing_factor": 1.10,
      "available": true
    }
  ],
  "default_model": "deepseek-v4-pro"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 模型 ID，用于 `POST /chat/completions` 的 `model` 字段 |
| `display_name` | string | UI 展示名称 |
| `provider` | string | 模型提供商 |
| `capabilities` | []string | 能力标签：`chat`, `coding`, `tool_call`, `vision`, `reasoning` |
| `context_window` | int | 上下文窗口大小（tokens） |
| `supports_streaming` | bool | 是否支持 SSE 流式 |
| `supports_tool_calls` | bool | 是否支持 tool/function calling |
| `category` | string | 模型分类：`reasoning` / `coding` / `fast` / `vision` |
| `recommended_for` | []string | 推荐使用场景，由 Gateway 控制，客户端无需硬编码 |
| `input_billing_factor` | float | 输入 token 的计费系数（billing factor） |
| `output_billing_factor` | float | 输出 token 的计费系数 |
| `available` | bool | 当前用户是否可用 |

> **设计原则：** `category`、`recommended_for`、`available` 均由 Gateway 控制。未来模型增删、推荐场景变更，客户端不需要升级。
>
> **`billing_factor` 语义：** 不是价格（价格是 estimated_cost），而是 quota 消耗权重。1 input token × input_billing_factor + 1 output token × output_billing_factor = 消耗的 billing units。此字段不绑定具体模型定价，同一模型降价时只改内部 cost 计算，不影响用户 Quota 消耗速度。

---

## 6. Usage API

> **调用方：** AgentKit (Swift)
>
> **不应调用方：** Code-Agent Runtime — Runtime 不展示 UI 用量信息。

### 6.1 获取用量

```
GET /api/v1/agent/usage
Authorization: Bearer <access_token>
```

**Response `data`:**

```json
{
  "tier": "pro",
  "mode": "managed",
  "daily": {
    "units_used": 125000,
    "units_limit": 2500000,
    "tokens_used": 85000,
    "utilization_pct": 5.0,
    "resets_at": "2026-07-10T00:00:00+08:00"
  },
  "weekly": {
    "units_used": 1250000,
    "units_limit": 12500000,
    "tokens_used": 850000,
    "utilization_pct": 10.0,
    "resets_at": "2026-07-13T00:00:00+08:00"
  },
  "monthly": {
    "units_used": 3500000,
    "units_limit": 50000000,
    "tokens_used": 2300000,
    "utilization_pct": 7.0,
    "resets_at": "2026-08-01T00:00:00+08:00"
  },
  "by_model": [
    {
      "model": "deepseek-v4-pro",
      "units_used": 2800000,
      "tokens_used": 1800000,
      "call_count": 145
    },
    {
      "model": "deepseek-v4-flash",
      "units_used": 700000,
      "tokens_used": 500000,
      "call_count": 230
    }
  ]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `tier` | string | 当前订阅等级：`free` / `pro` / `power` / `ultimate` |
| `mode` | string | `"managed"` 或 `"byok"` |
| `*.units_used` | int64 | 已消耗 Usage Units |
| `*.units_limit` | int64 | 周期内配额上限 |
| `*.utilization_pct` | float | 使用百分比 0-100 |
| `*.resets_at` | string | 配额重置时间（RFC3339） |
| `by_model` | array | 按月维度的各模型用量分布 |

### 6.2 BYOK 用量上报

```
POST /api/v1/agent/usage/report
Authorization: Bearer <access_token>
```

> **P2 优先级，默认关闭。** 仅当用户主动在端侧开启 `usage_reporting.enabled = true` 时调用。不影响配额。

**Request Body:**

```json
{
  "execution_id": "exec-xyz789",
  "session_id": "sess-abc123",
  "provider": "openai",
  "model": "gpt-5.3-codex",
  "input_tokens": 3500,
  "output_tokens": 800,
  "tool_calls_count": 3,
  "duration_ms": 4500
}
```

**Response `data`:** `{ "recorded": true, "record_id": "rec-xyz789" }`

---

## 7. Client 角色分工

这是三方之间的职责边界，必须严格遵守：

```
                    Agent Gateway API
                   (本文档定义的契约)
                          │
         ┌────────────────┼────────────────┐
         │                │                │
     AgentKit         Code-Agent          CLI
     (Swift)          (Go Runtime)        (Go)
         │                │                │
         │                │                │
    调用的接口:       调用的接口:        调用的接口:
    ───────────      ───────────       ───────────
    /auth/*           /agent/chat       /auth/*
    /user/profile     /agent/models     /agent/chat
    /agent/usage                        /agent/models
    /agent/models                       /agent/usage
    /billing/*
```

| API 分类 | AgentKit | Code-Agent | CLI |
|----------|:--------:|:----------:|:---:|
| `/auth/*` | ✅ | ❌ | ✅ |
| `/user/profile` | ✅ | ❌ | ❌ |
| `/agent/chat/completions` | ❌ | ✅ | ✅ |
| `/agent/models` | ✅ | ✅ | ✅ |
| `/agent/usage` | ✅ | ❌ | ✅ |
| `/agent/usage/report` | ❌ | ⚠️ P2 | ⚠️ P2 |
| `/billing/*` | ✅ | ❌ | ❌ |

**核心原则：**

1. **Runtime 不处理认证。** Token 由宿主（AgentKit/CLI）注入。Runtime 只需要 `{endpoint, token}` 两个参数。
2. **Runtime 不知道 JWT 结构。** 它只是一个 opaque bearer token。
3. **Runtime 不展示用量 UI。** 用量数据属于 Account 层面，由 AgentKit 处理。
4. **Runtime 不知道模型定价。** `billing_factor` 由 UI 层用于展示"该模型消耗更多额度"的提示。

---

## 8. 错误码参考

### 8.1 通用错误码

| Code | 说明 |
|------|------|
| 0 | 成功 |
| 400 | 参数校验失败 |
| 401 | 未登录或 Token 过期 |

### 8.2 Auth 错误码

| Code | 说明 |
|------|------|
| 40001 | 用户名或密码错误 |
| 40002 | 验证码错误 |
| 40003 | 手机号已被绑定 |
| 40004 | Apple 登录失败 |
| 40005 | Token 无效 |

### 8.3 Agent 错误码

| Code | 说明 |
|------|------|
| 42001 | 配额不足（日/周/月） |
| 42002 | 模型不存在 |
| 42003 | Provider 调用失败 |
| 42004 | 模型不支持流式 |
| 42005 | 请求参数错误 |
| 42006 | BYOK 用量上报失败 |

### 8.4 Billing 错误码

| Code | 说明 |
|------|------|
| 41001 | 点数不足 |
| 41002 | 权益不足 |
| 41003 | 产品不存在 |
| 41004 | 订单验证失败 |
| 41005 | 订单重复 |

---

## 附录 A：OpenAPI Spec

完整的 OpenAPI 3.0 规范见：

> `docs/api/openapi.yaml`

可使用 Swagger Editor 或 Redoc 渲染。

---

## 附录 B：Token 结构参考

Gateway 签发的 JWT payload 结构（客户端**不应依赖**此结构，仅供调试）：

```json
{
  "sub": "10001",
  "iat": 1750000000,
  "exp": 1750432000,
  "role": 2,
  "type": "access",
  "account_type": "registered",
  "device_id": "device-xxx"
}
```

> **契约保证：** Token 格式和字段可能随版本变化。客户端唯一应依赖的行为是：将此 token 放在 `Authorization: Bearer <token>` Header 中发送。

---

> **文档版本：** v1.0
>
> **状态：** Frozen — 三方契约
>
> **维护方：** Agent Gateway Team
>
> **变更流程：** 任何对本文档的修改需同步更新 `openapi.yaml` 并通知 AgentKit 和 Code-Agent 团队。
