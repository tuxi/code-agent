package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// Opt-in verification against a real repository. It only reads Git objects and
// never writes, resets, or checks out anything.
//
//	CODEAGENT_OBSERVE_REPO=/path/to/repo \
//	CODEAGENT_OBSERVE_BASELINE=<sha> CODEAGENT_OBSERVE_DELIVERY=<sha> \
//	go test ./internal/mcpbridge -run TestObserveRealRepositoryRange -v
func TestObserveRealRepositoryRange(t *testing.T) {
	repo := os.Getenv("CODEAGENT_OBSERVE_REPO")
	baseline := os.Getenv("CODEAGENT_OBSERVE_BASELINE")
	delivery := os.Getenv("CODEAGENT_OBSERVE_DELIVERY")
	if repo == "" || baseline == "" || delivery == "" {
		t.Skip("explicit read-only observation fixture required")
	}
	ctx := context.Background()
	c, err := newClient(ctx, "http://127.0.0.1:9", strings.Repeat("t", 32), repo, baseline, delivery)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	diff, truncated, err := c.rangeDiff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("range diff was truncated")
	}
	if strings.TrimSpace(diff) == "" {
		t.Fatal("range diff is empty; a delivered change must not look like no change")
	}
	files, cut, err := c.rangeFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cut || len(files) == 0 {
		t.Fatalf("changed files = %v truncated=%v", files, cut)
	}
	head, dirty := c.headState(ctx)
	sum := sha256.Sum256([]byte(diff))
	t.Logf("range=%s..%s files=%d diff_sha256=%s head=%s head_moved=%v worktree_dirty=%v",
		baseline, delivery, len(files), hex.EncodeToString(sum[:]), head, head != delivery, dirty)
	for _, f := range files {
		t.Logf("  changed: %s", f)
	}
}
