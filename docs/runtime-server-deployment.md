# Runtime Server Connections：部署与安全

CodeAgent Runtime Server 可以由 Talkify 嵌入，也可以作为 Local / Remote
Server 独立运行。两种形态都使用相同的 HTTP API 和 Agent Wire 协议，但
Server Access Token 与模型 Provider Credential 是两套完全独立的凭证。

## Embedded Runtime

Embedded Runtime 必须由宿主在每次启动时生成一个新的 256-bit 随机 Token，
通过 `CodeAgentRuntime` 的 `serverAccessToken` 参数传入：

- Token 只驻留在 AgentKit 与 Runtime 进程内存中；
- 不写入 YAML、UserDefaults、Keychain、数据库或日志；
- AgentKit 创建的 Embedded RuntimeClient 为 HTTP 和 WebSocket 自动添加
  `Authorization: Bearer <token>`；
- Runtime 重启后旧 Token 立即失效；
- `/healthz` 可公开，其他 Info、Models、Capabilities、会话、资产、Activity
  和 Agent Wire 路径都必须通过认证。

动态 loopback 端口不是安全边界。即使 Runtime 只监听 `127.0.0.1`，也不能
关闭 Embedded 认证。

## Embedded Runtime 局域网共享

Embedded Runtime 的私有 Listener 始终只允许绑定 loopback。Mac AgentKit
可以通过 `CodeAgentRuntime.Server.startSharedListener` 启动第二个、默认关闭
的 TLS Listener。两个 Listener 复用同一个 Runtime Handler、会话数据库、
执行器和 `server_id`，但使用完全隔离的认证材料：

```text
Loopback Listener -> Embedded 临时 Token
Shared Listener   -> 每台设备独立的 Device Credential
```

`startSharedListener` 接收的 JSON 全部是内存控制面参数：

```json
{
  "addr": "0.0.0.0:0",
  "certificate_pem": "-----BEGIN CERTIFICATE-----\n...",
  "private_key_pem": "-----BEGIN PRIVATE KEY-----\n...",
  "bootstrap_sha256": "<64-character lowercase hex>",
  "bootstrap_expires_at": 1785373200,
  "enrollment_timeout_seconds": 60,
  "devices": [
    {
      "device_id": "dev_...",
      "credential_sha256": "<64-character lowercase hex>"
    }
  ]
}
```

TLS 私钥、bootstrap secret 和 Device Credential 不得写入 Runtime YAML 或
日志。证书及私钥由 Mac AgentKit 在 Keychain 中管理；iPhone 必须对 HTTP 与
Agent Wire WebSocket 使用同一份 SPKI pin。

`sharedListenerStatus()` 返回：

```json
{
  "state": "running",
  "listen_address": "[::]:52134",
  "listen_origin": "https://[::]:52134",
  "port": 52134,
  "started_at": "2026-07-30T10:00:00Z",
  "last_transition_at": "2026-07-30T10:00:00Z",
  "last_error": ""
}
```

`state` 为 `stopped | starting | running | failed`。意外退出时状态切换为
`failed` 并保留脱敏后的最近错误。`listen_address` / `listen_origin` 只是绑定
诊断信息；wildcard `0.0.0.0` 或 `[::]` 不是客户端 Endpoint，禁止放入 Bonjour
TXT 或二维码。AgentKit 应使用 Bonjour 服务名、Mac `.local` 主机名和 `port`
生成客户端可连接地址。

Shared HTTP Server 使用 10 秒 `ReadHeaderTimeout`、90 秒 `IdleTimeout` 和
64 KiB Header 上限。配对请求体上限为 16 KiB，其他已认证请求体上限为
1 MiB；不设置会破坏 WebSocket/流式响应的全局 `WriteTimeout`。撤销设备会
拒绝后续 HTTP 请求，并关闭该设备已经建立的 Agent Wire WebSocket。

配对请求：

```http
POST /v1/runtime/pair
Content-Type: application/json

{
  "bootstrap_secret": "<256-bit one-time secret>",
  "device_name": "Xiaoyuan’s iPhone",
  "platform": "iOS"
}
```

Runtime 创建待确认 enrollment 后会保持该 HTTP 请求，不会立即返回
Credential。Mac AgentKit 通过以下嵌入接口完成持久化确认：

```text
pendingSharedEnrollments()
acknowledgeSharedEnrollment(enrollmentID)
rejectSharedEnrollment(enrollmentID)
```

`pendingSharedEnrollments()` 只返回 `enrollment_id`、code-agent 生成的
`device_id`、Credential SHA-256 和展示 metadata，不返回明文 Credential。
AgentKit 先把验证材料写入 Mac Keychain，再 acknowledge；只有收到确认后，
配对 HTTP 响应才会把明文 Credential 返回一次给 iPhone。超时、拒绝或连接
中断会撤销该 pending enrollment。

Device Credential 使用以下可定位格式：

```text
ca_device.<device_id>.<base64url-256-bit-secret>
```

Runtime 先定位 `device_id`，再对完整 Credential 计算 SHA-256 并使用
constant-time 比较。bootstrap 一次只能有一个 pending enrollment，成功后
立即消费。设备撤销通过 `updateSharedDevices` 热更新，不要求重启 Runtime。

## 独立 Local Server

本地开发可以生成一次至少 32 字节的随机 Token，保存在已被 Git 忽略的
`config.yaml` 中：

```bash
openssl rand -base64 32
```

对应配置：

```yaml
server:
  display_name: "My Mac"
  authentication: bearer
  access_token: "<上一步生成的固定 Token>"
  access_token_env: CODEAGENT_SERVER_ACCESS_TOKEN
  public_healthz: true
```

`access_token_env` 对应的环境变量有值时覆盖 `access_token`。因此 GoLand
等本地调试环境可以直接使用固定 YAML Token，生产部署仍可使用 Secret
Manager 注入环境变量。

Local Client 单独保存 Server Access Token。不要把它放进 Provider
Credential Registry，也不要把 Provider API Key 当作 Server Token 使用。
`config.yaml` 已被 `.gitignore` 排除；不要把真实 Token 写入
`config.example.yaml` 或提交到版本库。

## Remote Server：直接 TLS

监听非 loopback 地址时，CodeAgent 会拒绝以下任一配置：

- `authentication: none`；
- 未同时配置 TLS 证书和私钥。

```yaml
server:
  display_name: "Build Runtime"
  authentication: bearer
  # Remote/production 推荐不配置 access_token，只使用 Secret Manager 注入。
  access_token_env: CODEAGENT_SERVER_ACCESS_TOKEN
  public_healthz: true
  tls_certificate: "/etc/codeagent/server.crt"
  tls_private_key: "/etc/codeagent/server.key"
```

```bash
export CODEAGENT_SERVER_ACCESS_TOKEN="$(openssl rand -base64 32)"
codeagent serve 0.0.0.0:8797
```

Client URL 使用 `https://`，Agent Wire 使用 `wss://`。生产环境必须使用可验证
的证书；客户端不应提供“忽略 TLS 错误”开关。

## Remote Server：反向代理

也可以让 Caddy、Nginx、Tailscale Serve 或 SSH 隧道负责外部 TLS。此时
CodeAgent 必须只监听 loopback，代理到 `http://127.0.0.1:8797`，并保留
客户端的 `Authorization` Header 和 WebSocket Upgrade Header。

即使代理层另有登录，仍建议保留 CodeAgent Bearer Token。不要在 URL 查询
参数、代理访问日志或命令行参数中传递 Token。

## 认证错误契约

所有受保护的 HTTP 和 WebSocket 握手路径共用相同认证中间件：

```json
{
  "code": 40100,
  "msg": "unauthorized",
  "data": {
    "code": "runtime_auth_required",
    "message": "Server access token is required"
  }
}
```

- HTTP Status 固定为 `401`；
- 信封数字 `code` 固定为 `40100`；
- 缺少 Header 时 `data.code` 为 `runtime_auth_required`；
- Token 错误或认证方案不是 Bearer 时为 `runtime_auth_invalid`。

## 发现接口

认证成功后，客户端先读取：

- `GET /v1/runtime/info`：稳定 `server_id`、Runtime 版本、Agent Wire
  major/revision、运行形态；
- `GET /v1/runtime/models`：Server-scoped 模型目录与 revision；
- `GET /v1/runtime/capabilities`：当前真实能力。

`headless`、`full_desktop` 和 `sandboxed` 只描述运行形态。客户端是否展示某项
功能，仍以 capabilities 为准。
