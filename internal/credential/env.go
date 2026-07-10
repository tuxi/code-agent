package credential

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// defaultEnvMapping returns the conventional env var name for a target.
// Targets not in this map return "" (not handled).
func defaultEnvMapping(t Target) string {
	if t.Namespace != "llm" {
		return ""
	}
	upper := strings.ToUpper(t.Name)
	// Common keys: DEEPSEEK_API_KEY, OPENAI_API_KEY, DASHSCOPE_API_KEY, GLM_API_KEY
	return upper + "_API_KEY"
}

// EnvResolver reads credentials from environment variables.
//
// Default mapping rules (Namespace "llm" only):
//
//	{Namespace:"llm", Name:"deepseek"} → DEEPSEEK_API_KEY
//	{Namespace:"llm", Name:"openai"}   → OPENAI_API_KEY
//	{Namespace:"llm", Name:"qwen"}     → QWEN_API_KEY
//
// For non-llm namespaces or when Mapping is set, see Mapping.
//
// Credentials from EnvResolver never expire (ExpiresAt is nil). The Source
// field is set to "env:<VAR_NAME>".
type EnvResolver struct {
	// Mapping overrides the default target→env‑var mapping. When non-nil,
	// only targets explicitly listed here are resolved; the default rules
	// are not consulted. The map key is the env var name, and the value
	// is the list of targets that var serves.
	//
	// When nil, the default rules apply.
	Mapping map[string][]Target
}

// Resolve implements Resolver.
func (r *EnvResolver) Resolve(_ context.Context, target Target) (ResolvedCredential, error) {
	envName := r.envName(target)
	if envName == "" {
		return ResolvedCredential{}, nil
	}
	val, ok := os.LookupEnv(envName)
	if !ok || val == "" {
		return ResolvedCredential{}, nil
	}
	return ResolvedCredential{
		Credential: Credential{
			Type:   Bearer,
			Secret: val,
		},
		Source: fmt.Sprintf("env:%s", envName),
	}, nil
}

// envName returns the env var name for target, or "" if not handled.
func (r *EnvResolver) envName(target Target) string {
	if r.Mapping != nil {
		for envName, targets := range r.Mapping {
			for _, t := range targets {
				if t == target {
					return envName
				}
			}
		}
		return ""
	}
	return defaultEnvMapping(target)
}
