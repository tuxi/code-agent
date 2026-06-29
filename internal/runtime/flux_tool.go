package runtime

import (
	"net/http"
	"os"
	"time"

	flux "flux"
	fluxmodel "flux/model"
	fluxtool "flux/tool"
	builtin "flux/tool/builtin"

	"code-agent/internal/tools"
)

// RegisterFluxTool 将 Flux Workflow Engine 作为原生 Tool 注册到 code-agent。
// Tool 嵌入模式：同进程，共享 LLM provider，事件可桥接。
func RegisterFluxTool(registry *tools.Registry) {
	// 1. 创建 flux 的工具注册表（DAG 节点可用的工具）
	fluxReg := fluxtool.NewRegistry()
	fluxReg.Register(builtin.NewMergeResultTool())

	// shell tool: 使用当前工作目录
	if wd, err := os.Getwd(); err == nil {
		fluxReg.Register(builtin.NewShellTool(wd))
	}

	// 2. LLM provider — 用环境变量（和 MCP server 一致）
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	modelName := os.Getenv("LLM_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4-pro"
	}

	provider := &fluxmodel.OpenAICompatibleProvider{
		BaseURL:    baseURL,
		APIKey:     os.Getenv("LLM_API_KEY"),
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
	}

	// 3. 创建 FluxWorkflowTool
	wt := flux.NewWorkflowTool(flux.WorkflowToolConfig{
		Provider:  provider,
		ModelName: modelName,
		ToolReg:   fluxReg,
	})

	// 4. 包装为 code-agent Tool 并注册
	if err := registry.Register(tools.NewFluxWorkflowAdapter(wt)); err != nil {
		// 非致命：Flux tool 注册失败不阻止 code-agent 启动
		return
	}
}
