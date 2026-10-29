# OpenCode Go API 兼容性

核实日期：2026-10-29。

## 重试等待

Chat Completions 和 Responses 的普通调用、流式调用以及 HTTP 200 中的内嵌错误均保留服务端等待提示。

- 优先使用有效的 `Retry-After` 响应头，支持整数秒数和 HTTP 日期。
- 缺失或无效时，识别结构化错误消息中的 `after 30 s`、`retry after 30 seconds`、`retry in 500 ms` 等带单位的提示。
- 普通重试等待取本地指数退避与服务端等待的较大值；`max_backoff_seconds` 只限制本地退避，不能缩短服务端要求。
- 流式失败切换为非流式请求之前也遵守服务端等待，通过 `model_retrying.retry_delay_ms` 和重试日志报告。
- 等待可被调用方取消；重试次数、永久错误与额度耗尽的分类规则保持现有行为。

## 地址区别

| 地址 | 用途 |
| --- | --- |
| `https://opencode.ai/zen/go/v1` | 旧兼容入口；官方说明仍支持、路由至同一后端，尚未宣布移除日期 |
| `https://opencode.ai/inference/go/openai/v1` | 当前推荐的 OpenAI 协议入口；追加 `/chat/completions` 或 `/responses` |
| `https://opencode.ai/inference/go/anthropic/v1` | Anthropic Messages 协议；追加 `/messages` |
| `https://opencode.ai/inference/go/google/v1beta` | Gemini 协议；模型和方法置于路径中 |
| `https://opencode.ai/inference/go/v1/models` | 模型列表；不应从生成接口的协议前缀推导此地址 |
| `https://opencode.ai/inference/go/v1/usage` | 用量查询 |

`/inference/go/v1` 作为统一生成入口也仍为兼容保留，新接入应选择协议专用入口。当前 Harnesses 文档说明各协议可通过服务端转换访问其他协议的文本模型；这不意味着任意路径均可互换，也不代表所有参数均由 CodeAgent 支持。

CodeAgent 内置 OpenCode Go 默认地址改为 `/inference/go/openai/v1`，同时适配现有 Chat Completions 和 Responses provider。显式配置的 `base_url`（包括旧入口及代理）继续优先，不自动改写持久化设置。

新旧入口使用同一后端，因此切换地址不能解决 `ttft_admission_refused` 的上游容量不足。新入口的协议分流与重试等待是两项独立的兼容处理。

本次依据官方文档和维护者确认核实兼容关系，回归测试使用本地模拟 HTTP 服务，未使用真实 API key 进行付费推理验证。

## 验证

- 通过：`go test -race ./internal/model`；覆盖响应头、消息兜底、流式回退、HTTP 200 内嵌错误和等待取消。
- 通过：`go test ./internal/app -run 'TestOpenCodeGoEndpointDefaults|TestFromSettingsPerModelAPIOverride'`；覆盖默认新入口和显式旧地址、代理配置优先。
- 通过：`go build ./...`、`go vet ./...`、`git diff --check`。
- 扩大检查未全绿：app 包修改前已有 4 个 DeepSeek 注册相关失败；agent、conversation、runtime、server 包另有 11 个失败。通过 Go overlay 映射到本次改动前的文件快照复跑，这 11 个失败全部重现，失败集合相同；未修改这些测试或相关实现。

## 官方来源

- [Harnesses：Base URLs、协议转换及会话头](https://opencode.ai/v2/docs/console/harnesses#base-urls)
- [Go：模型及 API 入口](https://opencode.ai/v2/docs/console/go/#endpoints)
- [官方仓库 #52227：2026-10-21 的兼容入口说明及维护者确认](https://github.com/anomalyco/opencode/issues/52227)
