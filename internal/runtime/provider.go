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
// the provider resolves its credential dynamically via cred.Resolve().
// When cred is nil (backward compat), the provider falls back to the static
// APIKey field.
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
			// When cred is nil (CLI mode), fall back to a plain EnvResolver
			// so the credentials section still works.
			c := cred
			if c == nil {
				c = &credential.EnvResolver{}
			} else {
				c = &credential.ChainResolver{
					Resolvers: []credential.Resolver{c, &credential.EnvResolver{}},
				}
			}
			inner = model.NewOpenAICompatibleProvider(mc.BaseURL, c, mc.Credential.Target())
			// Propagate the resolved APIKey as a static fallback so injectSecrets
			// and env-var resolution from config-load time still take effect.
			if p, ok := inner.(*model.OpenAICompatibleProvider); ok {
				p.APIKey = mc.APIKey
			}
		} else {
			inner = model.NewOpenAICompatibleProviderWithKey(mc.BaseURL, mc.APIKey)
		}
	case "ollama":
		inner = model.NewOllamaProvider(mc.BaseURL)
	default:
		return nil, fmt.Errorf("unsupported provider %q (supported: \"openai\", \"ollama\")", mc.Provider)
	}
	return &model.ResilientProvider{
		Inner:      inner,
		MaxRetries: pc.MaxRetries,
		Timeout:    time.Duration(pc.RequestTimeoutSeconds) * time.Second,
		Backoff:    time.Duration(pc.BackoffMillis) * time.Millisecond,
		MaxBackoff: time.Duration(pc.MaxBackoffSeconds) * time.Second,
	}, nil
}
