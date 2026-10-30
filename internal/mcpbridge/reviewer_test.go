package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/collab"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func listToolNames(t *testing.T, server *mcp.Server) []*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "reviewer-test"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return list.Tools
}

// A reviewer connection must not even advertise a mutating capability. Hiding
// write tools is the first boundary; the Client guards below are the second.
func TestReviewerSurfaceIsReadOnly(t *testing.T) {
	tools := listToolNames(t, ReviewerServer(NewClientReviewReader(&Client{})))
	want := map[string]bool{
		"codeagent_connection_info":    true,
		"codeagent_read_session":       true,
		"codeagent_wait_for_update":    true,
		"codeagent_get_handoff":        true,
		"codeagent_get_artifacts":      true,
		"codeagent_read_task":          true,
		"codeagent_read_review_bundle": true,
	}
	if len(tools) != len(want) {
		var names []string
		for _, tool := range tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("reviewer surface has %d tools, want %d: %v", len(tools), len(want), names)
	}
	for _, tool := range tools {
		if !want[tool.Name] {
			t.Fatalf("reviewer surface exposes unexpected tool %q", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s is not annotated read-only", tool.Name)
		}
		for _, forbidden := range []string{"send", "create", "cancel", "run", "report", "link"} {
			if strings.Contains(strings.TrimPrefix(tool.Name, "codeagent_"), forbidden) && tool.Name != "codeagent_read_review_bundle" {
				t.Fatalf("reviewer surface exposes mutating tool %q", tool.Name)
			}
		}
	}
	// The executor surface must keep its write tools; the reviewer profile is
	// additive, not a replacement.
	executor := listToolNames(t, Server(&Client{}))
	found := false
	for _, tool := range executor {
		if tool.Name == "codeagent_send_to_session" {
			found = true
		}
	}
	if !found {
		t.Fatal("executor surface lost codeagent_send_to_session")
	}
	if len(executor) <= len(tools) {
		t.Fatalf("executor surface (%d) should remain larger than the reviewer surface (%d)", len(executor), len(tools))
	}
}

// Registration is not the only boundary: every mutating method must refuse in
// observation mode, even if a future code path calls it directly.
func TestObservationModeRefusesEveryWritePath(t *testing.T) {
	ctx := context.Background()
	c := &Client{observeOnly: true, id: "20261030-052252-b8f78ce8"}
	if _, err := c.Create(ctx, "req-1"); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Send(ctx, c.BoundSessionID(), "req-1", "do work", ""); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("Send: %v", err)
	}
	if err := c.Cancel(ctx, c.BoundSessionID()); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := c.AttachEmptySession(ctx, "other"); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("AttachEmptySession: %v", err)
	}
	if _, err := c.CreateTask(ctx, "req-1", "goal", nil); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := c.ReportTask(ctx, "task-1", "review", collab.ReportInput{}); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("ReportTask: %v", err)
	}
	if _, err := c.StartTaskRun(ctx, "task-1", collab.StartRunInput{}); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("StartTaskRun: %v", err)
	}
	if _, err := c.ReviewTaskRun(ctx, "task-1", "run-1", collab.ReviewInput{}); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("ReviewTaskRun: %v", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The reviewed evidence is baseline..delivery. Later commits and uncommitted
// edits are reported separately and must never leak into the diff.
func TestCommitRangeEvidenceIgnoresCurrentHeadAndWorktree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-q", "-m", "baseline")
	baseline := gitRun(t, dir, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "thumbnail.go"), "package main\n// delivered change\n")
	gitRun(t, dir, "add", "thumbnail.go")
	gitRun(t, dir, "commit", "-q", "-m", "deliver thumbnails")
	delivery := gitRun(t, dir, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "unrelated.go"), "package main\n// merged after delivery\n")
	gitRun(t, dir, "add", "unrelated.go")
	gitRun(t, dir, "commit", "-q", "-m", "merge later work")
	head := gitRun(t, dir, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "dirty.txt"), "uncommitted\n")

	c, err := newClient(ctx, "http://127.0.0.1:9", strings.Repeat("t", 32), dir, baseline, delivery)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.observeOnly {
		t.Fatal("observation binding did not set the read-only mode")
	}
	if c.baseline != baseline || c.delivery != delivery {
		t.Fatalf("binding pinned %s..%s, want %s..%s", c.baseline, c.delivery, baseline, delivery)
	}
	diff, truncated, err := c.rangeDiff(ctx)
	if err != nil || truncated {
		t.Fatalf("rangeDiff: %v truncated=%v", err, truncated)
	}
	if !strings.Contains(diff, "thumbnail.go") {
		t.Fatalf("delivered change missing from the range diff:\n%s", diff)
	}
	if strings.Contains(diff, "unrelated.go") {
		t.Fatal("a commit made after delivery leaked into the reviewed diff")
	}
	files, _, err := c.rangeFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "thumbnail.go" {
		t.Fatalf("changed files = %v, want [thumbnail.go]", files)
	}
	gotHead, dirty := c.headState(ctx)
	if gotHead != head {
		t.Fatalf("headState = %s, want %s", gotHead, head)
	}
	if !dirty {
		t.Fatal("headState did not report the dirty worktree")
	}
	if err := validateCommitRange(ctx, dir, delivery, baseline); err == nil {
		t.Fatal("a reversed range must be rejected")
	}
	if err := validateCommitRange(ctx, dir, baseline, strings.Repeat("0", 40)); err == nil {
		t.Fatal("an absent delivery commit must be rejected")
	}
}

// A third-party reviewer holds no credential: the bundle is a file plus a hash.
func TestEvidenceBundleIsFrozenVerifiedAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "case-001.json")
	baseline := strings.Repeat("a", 40)
	delivery := strings.Repeat("b", 40)
	b := EvidenceBundle{
		SchemaVersion: bundleSchemaVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		ExportedBy:    "test",
		SessionID:     "20261030-052252-b8f78ce8",
		TurnID:        "turn_18e33a6ab0de1858_45",
		Workspace:     "/tmp/workspace",
		Baseline:      baseline,
		Delivery:      delivery,
		Read:          ReadResult{SessionID: "20261030-052252-b8f78ce8", TurnID: "turn_18e33a6ab0de1858_45", Status: "completed", Cursor: 12, Terminal: true, Events: []Event{{Seq: 3, Kind: "turn_accepted", TurnID: "turn_18e33a6ab0de1858_45"}, {Seq: 9, Kind: "turn_finished", TurnID: "turn_18e33a6ab0de1858_45"}}},
		Artifacts:     Artifacts{SessionID: "20261030-052252-b8f78ce8", TurnID: "turn_18e33a6ab0de1858_45", Baseline: baseline, DeliveryCommit: delivery, EvidenceSource: "commit_range", Diff: "diff --git a/thumbnail.go", DiffSHA256: "deadbeef", EvidenceComplete: true},
		Handoff:       Handoff{SessionID: "20261030-052252-b8f78ce8", TurnID: "turn_18e33a6ab0de1858_45", Baseline: baseline, DeliveryCommit: delivery, EvidenceSource: "commit_range", ExecutionStatus: "completed", ValidationStatus: "partial", AcceptanceStatus: "not_reviewed", ChangedFiles: []string{"thumbnail.go"}},
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile0600(bundlePath, data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := writeFile0600(bundlePath+".sha256", []byte(hex.EncodeToString(sum[:])+"  case-001.json\n")); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenBundle(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.BindingInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.BundleVerified || info.Delivery != delivery || info.IdentityVerified {
		t.Fatalf("bundle binding = %+v", info)
	}
	r, err := reader.ReadSession(context.Background(), b.TurnID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Events) != 1 || r.Events[0].Seq != 9 {
		t.Fatalf("cursor filtering failed: %+v", r.Events)
	}
	if _, err := reader.ReadSession(context.Background(), "other-turn", 0); err == nil {
		t.Fatal("bundle must not serve an unrelated Turn")
	}
	h, err := reader.ReviewHandoff(context.Background(), b.TurnID, "")
	if err != nil || h.AcceptanceStatus != "not_reviewed" {
		t.Fatalf("handoff = %+v err=%v", h, err)
	}
	// Offline review must be served through the same read-only surface.
	if tools := listToolNames(t, ReviewerServer(reader)); len(tools) != 7 {
		t.Fatalf("bundle reviewer exposes %d tools", len(tools))
	}
	// Tampering must be detected rather than silently reviewed.
	tampered := []byte(strings.Replace(string(data), "thumbnail.go", "cover.go", 1))
	if err := os.WriteFile(bundlePath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBundle(bundlePath); err == nil {
		t.Fatal("a tampered bundle must be rejected")
	}
}
