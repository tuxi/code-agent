package runtime

import (
	"net/http"
	"os"
	"time"

	flux "github.com/tuxi/flux"
	fluxmodel "github.com/tuxi/flux/model"
	fluxstore "github.com/tuxi/flux/store"
	fluxtool "github.com/tuxi/flux/tool"
	builtin "github.com/tuxi/flux/tool/builtin"

	"code-agent/internal/app"
	"code-agent/internal/tools"
)

// FluxStoreSet holds the SQLite-backed Store implementations for flux.
// nil fields mean in-memory fallback is used.
type FluxStoreSet struct {
	WorkflowStore fluxstore.WorkflowStore // nil → in-memory
	AwaitStore    fluxstore.AwaitStore    // nil → in-memory
}

// RegisterFluxTool 将 Flux Workflow Engine 作为原生 Tool 注册到 code-agent。
// mc 是 code-agent 已解析的主模型配置（BaseURL/Model/APIKey 来自 config.yaml，
// APIKey 已由 api_key_env 解析）——flux 复用它，保证和 code-agent 用同一套 LLM 凭证，
// 不再各读一套互不相干的 LLM_* 环境变量（那正是「config.yaml 设了 key 却读不到」的根因）。
// stores: optional SQLite-backed stores (nil = in-memory fallback).
func RegisterFluxTool(registry *tools.Registry, mc app.ModelConfig, stores *FluxStoreSet) {
	// 1. 创建 flux 的工具注册表（DAG 节点可用的工具）
	fluxReg := fluxtool.NewRegistry()
	fluxReg.Register(builtin.NewMergeResultTool())

	// shell tool: 使用当前工作目录
	if wd, err := os.Getwd(); err == nil {
		fluxReg.Register(builtin.NewShellTool(wd))
	}

	// 2. LLM provider — 以 config.yaml 解析出的主模型为准；LLM_* 环境变量仅作可选覆盖。
	baseURL := mc.BaseURL
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		baseURL = v
	}
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	modelName := mc.Model
	if v := os.Getenv("LLM_MODEL"); v != "" {
		modelName = v
	}
	if modelName == "" {
		modelName = "deepseek-chat"
	}
	apiKey := mc.APIKey
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		apiKey = v
	}

	provider := &fluxmodel.OpenAICompatibleProvider{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
	}

	// 3. 创建 FluxWorkflowTool（注入可选的 Store）
	cfg := flux.WorkflowToolConfig{
		Provider:  provider,
		ModelName: modelName,
		ToolReg:   fluxReg,
	}
	if stores != nil {
		cfg.WFStore = stores.WorkflowStore
		cfg.AwaitStore = stores.AwaitStore
	}
	wt := flux.NewWorkflowTool(cfg)

	// 4. 包装为 code-agent Tool 并注册
	if err := registry.Register(tools.NewFluxWorkflowAdapter(wt)); err != nil {
		return // 非致命
	}
}
