package fixturetest

import (
	"path/filepath"
	"testing"
)

func TestFrozenPreviewFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
	report, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Scenarios) != 16 {
		t.Fatalf("fixture runner executed %d scenarios, want 16", len(report.Scenarios))
	}
}
