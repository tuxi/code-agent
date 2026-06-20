package main

import "testing"

func TestComputeCost(t *testing.T) {
	// 1,000,000 prompt @ 0.50/M + 500,000 completion @ 1.50/M = 0.50 + 0.75 = 1.25.
	if got := computeCost(1_000_000, 0, 500_000, 0.50, 0, 1.50); got != 1.25 {
		t.Fatalf("computeCost = %v, want 1.25", got)
	}
	// Unpriced model -> zero cost.
	if got := computeCost(12345, 0, 6789, 0, 0, 0); got != 0 {
		t.Fatalf("computeCost with no prices = %v, want 0", got)
	}
}

func TestComputeCostSplitsCachedTokens(t *testing.T) {
	// 1M prompt, 500k of it cached: 500k @ 1.0/M + 500k @ 0.5/M = 0.50 + 0.25 = 0.75.
	if got := computeCost(1_000_000, 500_000, 0, 1.0, 0.5, 0); got != 0.75 {
		t.Fatalf("split cost = %v, want 0.75", got)
	}
}

func TestComputeCostCacheFallsBackToInputPrice(t *testing.T) {
	// Cache price unset (0) must bill cached tokens at the full input price, so the
	// total equals the pre-cache estimate — never a silent under-count.
	withCache := computeCost(1_000_000, 1_000_000, 0, 0.5, 0, 0)
	noCacheInfo := computeCost(1_000_000, 0, 0, 0.5, 0, 0)
	if withCache != noCacheInfo || withCache != 0.5 {
		t.Fatalf("unset cache price should match the full-input estimate: %v vs %v", withCache, noCacheInfo)
	}
}

func TestComputeCostClampsCachedToPrompt(t *testing.T) {
	// A provider reporting more cached than prompt tokens must not go negative.
	if got := computeCost(1_000_000, 2_000_000, 0, 1.0, 0.5, 0); got != 0.5 {
		t.Fatalf("clamped cost = %v, want 0.5", got)
	}
}
