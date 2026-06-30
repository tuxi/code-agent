package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestEvals(t *testing.T) {
	if os.Getenv("CODEAGENT_EVAL") == "" {
		t.Skip("skipping eval tests (set CODEAGENT_EVAL=1 to run; requires model API calls)")
	}

	cases, err := LoadCases(filepath.Join("evals.json"))
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no eval cases found")
	}

	h, err := NewHarness()
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}

	ctx := context.Background()
	passed := 0
	failed := 0

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			result := h.Run(ctx, c)
			if result.Error != "" {
				t.Logf("ERROR: %s", result.Error)
			}
			if len(result.Response) > 0 {
				t.Logf("RESPONSE:\n%s", result.Response)
			}
			t.Logf("elapsed: %v", result.Elapsed)

			if !result.Passed {
				for _, m := range result.Missing {
					t.Errorf("MISSING marker: %q", m)
				}
				for _, m := range result.Forbidden {
					t.Errorf("FORBIDDEN marker found: %q", m)
				}
				failed++
			} else {
				passed++
			}
		})
	}

	fmt.Printf("\n--- Eval summary: %d/%d passed ---\n", passed, passed+failed)
	if failed > 0 {
		t.Fatalf("%d eval case(s) failed", failed)
	}
}
