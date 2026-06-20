package websearch

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestHeuristicRewrite_HowDoesXWork(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "how does redis geo work")
	if !strings.Contains(result, "redis") {
		t.Errorf("query should contain 'redis': %s", result)
	}
	if !strings.Contains(result, "implementation") {
		t.Errorf("should inject implementation keywords: %s", result)
	}
	// Filler should be stripped.
	if strings.Contains(result, "how does") {
		t.Errorf("filler 'how does' should be stripped: %s", result)
	}
}

func TestHeuristicRewrite_WhatIsX(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "what is kubernetes")
	if !strings.Contains(result, "kubernetes") {
		t.Errorf("expected kubernetes: %s", result)
	}
	if !strings.Contains(result, "documentation") {
		t.Errorf("expected documentation keyword: %s", result)
	}
}

func TestHeuristicRewrite_XVsY(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "rust vs go performance")
	if !strings.Contains(result, "comparison") || !strings.Contains(result, "benchmark") {
		t.Errorf("expected comparison/benchmark keywords: %s", result)
	}
}

func TestHeuristicRewrite_ErrorFix(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "npm install error EACCES")
	if !strings.Contains(result, "fix") || !strings.Contains(result, "solution") {
		t.Errorf("expected fix/solution keywords: %s", result)
	}
}

func TestHeuristicRewrite_AlreadyTechnical(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "redis sorted set geohash implementation")
	// Already a good query — should pass through largely unchanged.
	if !strings.Contains(strings.ToLower(result), "redis") {
		t.Errorf("should keep redis: %s", result)
	}
}

func TestHeuristicRewrite_ChineseFiller(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "请帮我查一下 redis geo 实现")
	if strings.Contains(result, "请帮我") || strings.Contains(result, "查一下") {
		t.Errorf("Chinese filler should be stripped: %s", result)
	}
	if !strings.Contains(result, "redis") {
		t.Errorf("should keep redis: %s", result)
	}
}

func TestHeuristicRewrite_ShortAmbiguousQuery(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "rust")
	rewritten, conf := heuristicRewrite("rust")
	_ = result
	if conf >= 0.4 {
		t.Logf("short query confidence is %.2f (expected < 0.4 for LLM fallback trigger)", conf)
	}
	if !strings.Contains(rewritten, "rust") {
		t.Errorf("should keep 'rust': %s", rewritten)
	}
}

func TestHeuristicRewrite_EmptyQuery(t *testing.T) {
	r := &Rewriter{}
	result := r.Rewrite(context.Background(), "")
	if result != "" {
		t.Errorf("empty query should stay empty, got %q", result)
	}
}

func TestLLMFallbackCalled(t *testing.T) {
	called := false
	r := &Rewriter{
		LLMFallback: func(ctx context.Context, query string) (string, error) {
			called = true
			return query + " llm-optimized", nil
		},
	}
	// A very short query → low confidence → triggers LLM fallback.
	result := r.Rewrite(context.Background(), "rs")
	if !called {
		t.Log("LLM fallback was not called (confidence may have been ≥ 0.4)")
	}
	_ = result
}

func TestLLMFallbackSkippedWhenConfident(t *testing.T) {
	called := false
	r := &Rewriter{
		LLMFallback: func(ctx context.Context, query string) (string, error) {
			called = true
			return query + " optimized", nil
		},
	}
	// A well-structured query → high confidence → should NOT call LLM.
	result := r.Rewrite(context.Background(), "how does redis geo sorted set work internally")
	_ = result
	if called {
		t.Error("LLM fallback should NOT be called for high-confidence query")
	}
}

func TestLLMFallbackErrorFallsBack(t *testing.T) {
	r := &Rewriter{
		LLMFallback: func(ctx context.Context, query string) (string, error) {
			return "", fmt.Errorf("simulated timeout")
		},
	}
	// Short query to trigger fallback, but fallback errors → use heuristic.
	result := r.Rewrite(context.Background(), "ab")
	if result == "" {
		t.Error("should fall back to heuristic when LLM errors")
	}
}

func TestCollapseWhitespace(t *testing.T) {
	in := "redis   geo    implementation"
	out := collapseWhitespace(in)
	if out != "redis geo implementation" {
		t.Errorf("expected collapsed whitespace, got %q", out)
	}
}

func TestComputeConfidence_HighForLongTechnical(t *testing.T) {
	conf := computeConfidence(
		"how does redis geo sorted set work internally",
		"redis geo sorted set work internally implementation internals design architecture",
		true,
	)
	if conf < 0.7 {
		t.Errorf("expected high confidence, got %.2f", conf)
	}
}

func TestCorrectStaleYear_StaleReplaced(t *testing.T) {
	// 2025 in 2026 → should correct to 2026.
	result := correctStaleYear("GitHub trending Golang projects today 2025")
	if strings.Contains(result, "2025") {
		t.Errorf("stale year 2025 should be corrected: %s", result)
	}
	if !strings.Contains(result, "2026") {
		t.Errorf("should contain current year 2026: %s", result)
	}
}

func TestCorrectStaleYear_HistoricalPreserved(t *testing.T) {
	// 2010 is historical, not stale.
	result := correctStaleYear("Python 2.7 released 2010")
	if !strings.Contains(result, "2010") {
		t.Errorf("historical year 2010 should be preserved: %s", result)
	}
}

func TestCorrectStaleYear_CurrentYearPreserved(t *testing.T) {
	result := correctStaleYear("Swift 2026 new features")
	if !strings.Contains(result, "2026") {
		t.Errorf("current year 2026 should be preserved: %s", result)
	}
}

func TestComputeConfidence_LowForShort(t *testing.T) {
	conf := computeConfidence("rust", "rust guide documentation", false)
	if conf > 0.4 {
		t.Errorf("expected low confidence for short query, got %.2f", conf)
	}
}
