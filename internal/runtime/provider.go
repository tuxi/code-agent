package runtime

import (
	"fmt"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/credential"
	"code-agent/internal/model"
)

// BuildProvider constructs a model.Provider from a resolved model config.
//
// When cred is non-nil and the model config has an explicit CredentialRef,
// the provider resolves its credential dynamically via cred.Resolve(). The
// caller passes the full effective resolver (session override → base chain);
// BuildProvider no longer wraps it in an EnvResolver (R1.3) — every non-nil
// cred built by Config.CredentialResolver already ends in an EnvResolver
// fallback, so re-wrapping was a redundant chain-in-chain. When cred is nil
// (CLI backward compat), the provider falls back to a plain EnvResolver so
// the credentials section still works.
//
// Every provider is wrapped in a ResilientProvider so a transient API error
// (timeout, 429, 5xx) does not kill the run: timeout and retry policy live in
// this one transport layer, not in each provider.
func BuildProvider(mc app.ModelConfig, pc app.ProviderConfig, cred credential.Resolver) (model.Provider, error) {
	var inner model.Provider
	switch mc.Provider {
	case "openai", "":
		if !mc.Credential.IsZero() {
			// Model has an explicit credential ref — use the dynamic path.
			c := cred
			if c == nil {
				c = &credential.EnvResolver{}
			}
			inner = model.NewOpenAICompatibleProvider(mc.BaseURL, c, mc.Credential.Target())
		} else {
			inner = model.NewOpenAICompatibleProviderWithKey(mc.BaseURL, "")
		}
	case "ollama":
		inner = model.NewOllamaProvider(mc.BaseURL)
	case "responses":
		// OpenAI Responses API (/v1/responses) — the newer OpenAI wire format,
		// natively supported by OpenAI and DeepSeek. Same credential handling
		// as the openai-compatible provider.
		if !mc.Credential.IsZero() {
			c := cred
			if c == nil {
				c = &credential.EnvResolver{}
			}
			inner = model.NewResponsesProvider(mc.BaseURL, c, mc.Credential.Target())
		} else {
			inner = model.NewResponsesProviderWithKey(mc.BaseURL, "")
		}
	default:
		return nil, fmt.Errorf("unsupported provider %q (supported: \"openai\", \"ollama\", \"responses\")", mc.Provider)
	}
	return &model.ResilientProvider{
		Inner:      inner,
		MaxRetries: pc.MaxRetries,
		Timeout:    time.Duration(pc.RequestTimeoutSeconds) * time.Second,
		Backoff:    time.Duration(pc.BackoffMillis) * time.Millisecond,
		MaxBackoff: time.Duration(pc.MaxBackoffSeconds) * time.Second,
	}, nil
}

// inheritProviderObserver keeps request telemetry attached when a turn or
// subagent builds a Provider for a non-default Runtime Alias.
func inheritProviderObserver(parent, child model.Provider) {
	parentResilient, parentOK := parent.(*model.ResilientProvider)
	childResilient, childOK := child.(*model.ResilientProvider)
	if parentOK && childOK {
		childResilient.Observer = parentResilient.Observer
		childResilient.LogWriter = parentResilient.LogWriter
	}
}
