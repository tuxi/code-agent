package runtime

import (
	"code-agent/internal/app"
	"code-agent/internal/model"
	"fmt"
	"time"
)

// BuildProvider constructs a model.Provider from a resolved model config. Only
// OpenAI-compatible endpoints are wired today; this is the extension point for
// Anthropic, Gemini, Ollama, etc.
//
// Every provider is wrapped in a ResilientProvider so a transient API error
// (timeout, 429, 5xx) does not kill the run: timeout and retry policy live in
// this one transport layer, not in each provider.
func BuildProvider(mc app.ModelConfig, pc app.ProviderConfig) (model.Provider, error) {
	var inner model.Provider
	switch mc.Provider {
	case "openai", "":
		inner = model.NewOpenAICompatibleProvider(mc.BaseURL, mc.APIKey)
	default:
		return nil, fmt.Errorf("unsupported provider %q (only \"openai\"-compatible is wired so far)", mc.Provider)
	}
	return &model.ResilientProvider{
		Inner:      inner,
		MaxRetries: pc.MaxRetries,
		Timeout:    time.Duration(pc.RequestTimeoutSeconds) * time.Second,
		Backoff:    time.Duration(pc.BackoffMillis) * time.Millisecond,
		MaxBackoff: time.Duration(pc.MaxBackoffSeconds) * time.Second,
	}, nil
}
