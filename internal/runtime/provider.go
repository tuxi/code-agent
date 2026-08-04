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
