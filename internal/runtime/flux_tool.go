package runtime

import (
	"net/http"
	"os"
	"strings"
	"time"

	flux "github.com/tuxi/flux"
	fluxtool "github.com/tuxi/flux-workflow/tool"
	fluxmodel "github.com/tuxi/flux/model"
	fluxstore "github.com/tuxi/flux/store"
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
// sandboxed: if true, the internal shell tool is not registered into flux's own tool
// registry (fork/exec is forbidden), so workflow steps only use merge_result. The
// plan_workflow tool itself requires no subprocess and remains registered.
func RegisterFluxTool(registry *tools.Registry, mc app.ModelConfig, stores *FluxStoreSet, sandboxed bool) {
	// 1. 创建 flux 的工具注册表（DAG 节点可用的工具）
	fluxReg := fluxtool.NewRegistry()
	fluxReg.Register(builtin.NewMergeResultTool())

	// shell tool: only on hosts that can fork/exec (desktop). On a sandboxed host
	// (iOS) flux workflow steps still work with merge_result; they just can't shell.
	if !sandboxed {
		if wd, err := os.Getwd(); err == nil {
			fluxReg.Register(builtin.NewShellTool(wd))
		}
	}

	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = mc.BaseURL
	}
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}

	if mc.Provider == "ollama" && !strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimRight(baseURL, "/") + "/v1"
	}

	modelName := os.Getenv("LLM_MODEL")
	if modelName == "" {
		modelName = mc.Model
	}

	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		apiKey = mc.APIKey
	}
	// Local providers (Ollama) don't require an API key, but flux's provider
	// rejects an empty key. Pass a dummy value so flux can make calls.
	if apiKey == "" && mc.Provider == "ollama" {
		apiKey = "ollama"
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
