package credential

import "testing"

func TestMutableResolverMergeAllPreservesExistingCredentials(t *testing.T) {
	r := NewMutableResolver()
	deepseek := Target{Namespace: "llm", Name: "deepseek"}
	qwen := Target{Namespace: "llm", Name: "qwen"}
	r.SetAll(map[Target]Credential{
		deepseek: {Type: Bearer, Secret: "deepseek-key"},
	})
	r.MergeAll(map[Target]Credential{
		qwen: {Type: Bearer, Secret: "qwen-key"},
	})
	if got, _ := r.Resolve(t.Context(), deepseek); got.Secret != "deepseek-key" {
		t.Fatalf("deepseek credential = %q, want preserved credential", got.Secret)
	}
	if got, _ := r.Resolve(t.Context(), qwen); got.Secret != "qwen-key" {
		t.Fatalf("qwen credential = %q, want merged credential", got.Secret)
	}
}
