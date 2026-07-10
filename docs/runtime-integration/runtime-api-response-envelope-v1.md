# Runtime API Response Envelope v1

> **Status**: Implemented — 2026-07-10. AgentKit 需要适配。
> **适用范围**: Runtime HTTP API 所有端点（WebSocket 不受影响）。

---

## 1. 变更概述

Runtime HTTP API 的所有 JSON 响应现在统一包裹在 ApiResponse 信封中，格式与 Agent Gateway 的 `/models`、`/usage`、`/auth/*` 端点完全一致。

**不受影响的端点**：
- `GET /healthz` — 仍然返回纯文本 `ok`
- `GET /v1/conversations/{id}/stream` (WebSocket) — 裸 wire frame，无信封
- `GET /v1/jobs/{id}/stream` (WebSocket) — 同上

---

## 2. 信封格式

### 成功响应

```json
{
  "trace_id": "uuid",
  "code": 0,
  "msg": "success",
  "data": <原始 payload>
}
```

### 错误响应

```json
{
  "trace_id": "uuid",
  "code": <HTTP status * 100>,
  "msg": "<错误描述>",
  "data": null
}
```

### 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `trace_id` | string | 请求追踪 UUID |
| `code` | int | `0` = 成功；非 0 = 业务错误码（`http_status * 100`） |
| `msg` | string | 人类可读描述。成功时为 `"success"` |
| `data` | any | 响应负载。成功时为原始 JSON；错误时为 `null`（clone 错误除外，见 §4） |

---

## 3. 各端点适配对照

### 3.1 `GET /v1/conversations`

**之前**：直接返回数组
```json
[{"id":"...","workspace_path":"...","name":"..."}]
```

**现在**：
```json
{"trace_id":"...","code":0,"msg":"success","data":[{"id":"...","workspace_path":"...","name":"..."}]}
```

**适配**：取 `response.data` 再 decode 为 `[ConversationRef]`。

---

### 3.2 `POST /v1/conversations`

**之前**：
```json
{"id":"...","workspace_path":"..."}
```
status: 201

**现在**：
```json
{"trace_id":"...","code":0,"msg":"success","data":{"id":"...","workspace_path":"..."}}
```
status: 201

**适配**：取 `response.data` 再 decode 为 `ConversationRef`。

---

### 3.3 `GET /v1/conversations/{id}`

**之前**：直接返回对象
```json
{"id":"...","workspace_path":"...","name":"...","turn_count":3,"message_count":6}
```

**现在**：包裹在 `data` 字段中。

**适配**：取 `response.data` 再 decode 为 `ConversationDetail`。

---

### 3.4 `GET /v1/conversations/{id}/messages`

**之前**：直接返回数组
```json
[{"seq":0,"role":"user","content":"hi"}]
```

**现在**：包裹在 `data` 字段中。

**适配**：取 `response.data` 再 decode 为 `[MessageView]`。

---

### 3.5 `GET /v1/conversations/{id}/events`

**之前**：直接返回数组
```json
[{"seq":1,"kind":"turn_started",...}]
```

**现在**：包裹在 `data` 字段中。

**适配**：取 `response.data` 再 decode 为 `[WireFrame]`。注意 WS 收到的 live events 仍然是裸 frame，无信封。

---

### 3.6 `GET /v1/conversations/{id}/events?since=N`

同上，增量 replay 也包裹在 `data` 中。

---

### 3.7 `GET /v1/jobs/{id}/events`

同 §3.5，包裹在 `data` 中。

---

### 3.8 Assets 端点

`GET /v1/conversations/{id}/assets/{asset_id}/preview`
`GET /v1/conversations/{id}/assets/{asset_id}/content`

成功时包裹在 `data` 中。`/blob` 返回原始二进制，无信封。`/thumbnail` 返回 501 错误，带信封。

---

### 3.9 `PATCH /v1/conversations/{id}`

成功响应包裹在 `data` 中，返回更新后的 `ConversationRef`。
错误（如名称为空）包裹在 `msg` 中，`data` 为 null。

---

### 3.10 `DELETE /v1/conversations/{id}`

状态码 204，无响应体。无信封。

---

### 3.11 `POST /v1/conversations/{id}/rebind`

状态码 204，无响应体。无信封。

---

### 3.12 `POST /v1/repos/clone`

成功（201）：包裹在 `data` 中，返回 `CloneRepoResponse`。
错误（400/500）：`code` 为 `status*100`，`data` 中包含 `{"error":"<code>","message":"<detail>"}` 供 UI 展示。

---

### 3.13 `GET /v1/prompts`

包裹在 `data` 中。

---

## 4. 客户端适配伪代码

```swift
// 通用响应解码
struct ApiEnvelope<T: Decodable>: Decodable {
    let traceId: String
    let code: Int
    let msg: String
    let data: T?
}

// 成功路径
let envelope = try decoder.decode(ApiEnvelope<T>.self, from: body)
guard envelope.code == 0 else {
    throw ApiError(code: envelope.code, message: envelope.msg)
}
return envelope.data

// 或直接用已有的 Agent Gateway ApiResponse 解析器（格式完全兼容）
```

## 5. 不变的部分

| 端点 | 格式 | 变更 |
|------|------|:--:|
| `GET /healthz` | 纯文本 `ok` | 无 |
| `GET /v1/conversations/{id}/stream` | WebSocket wire frame | 无 |
| `GET /v2/conversations/{id}/stream` | WebSocket wire frame | 无 |
| `GET /v1/jobs/{id}/stream` | WebSocket wire frame | 无 |
| `GET /v1/conversations/{id}/assets/{id}/blob` | 原始二进制 | 无 |
