package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/search"
)

// TestRunnerNativeLoop drives the rewritten loop end to end against a real
// model: the agent should call at least one tool and then produce a final text
// answer. Skipped unless DEEPSEEK_API_KEY is set.
func TestRunnerNativeLoop(t *testing.T) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live runner test")
	}
	baseURL := getenvOr("CODEAGENT_TEST_BASE_URL", "https://api.deepseek.com")
	modelName := getenvOr("CODEAGENT_TEST_MODEL", "deepseek-v4-flash")

	reg := tools.NewRegistry()
	mustRegister(t, reg, filesystem.NewListFilesTool("."))
	mustRegister(t, reg, filesystem.NewReadFileTool("."))
	mustRegister(t, reg, search.NewGrepTool("."))

	runner := &Runner{
		Model:     model.NewOpenAICompatibleProvider(baseURL, apiKey),
		ModelName: modelName,
		Tools:     reg,
		MaxSteps:  8,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	res, err := runner.Run(ctx, "List the files in the current directory, then tell me what this Go package contains.")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Final == "" {
		t.Fatal("expected a non-empty final answer")
	}

	var calledATool bool
	for _, s := range res.State.Steps {
		if s.ToolName != "" {
			calledATool = true
		}
	}
	if !calledATool {
		t.Errorf("expected the agent to call at least one tool; steps=%+v", res.State.Steps)
	}

	t.Logf("steps=%d final=%s", len(res.State.Steps), res.Final)
}

func getenvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
