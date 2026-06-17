package main

import "testing"

func TestComputeCost(t *testing.T) {
	// 1,000,000 prompt @ 0.50/M + 500,000 completion @ 1.50/M = 0.50 + 0.75 = 1.25.
	if got := computeCost(1_000_000, 500_000, 0.50, 1.50); got != 1.25 {
		t.Fatalf("computeCost = %v, want 1.25", got)
	}
	// Unpriced model -> zero cost.
	if got := computeCost(12345, 6789, 0, 0); got != 0 {
		t.Fatalf("computeCost with no prices = %v, want 0", got)
	}
}
