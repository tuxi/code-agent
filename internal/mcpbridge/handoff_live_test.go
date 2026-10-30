//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// This is an opt-in Go API diagnostic, not a Desktop MCP discovery test or an
// alternative stdio driver. It never submits input, approval or cancellation.
func TestExistingSessionHandoffReadOnly(t *testing.T) {
	manifest := os.Getenv("CODEAGENT_HANDOFF_READONLY_MANIFEST")
	if manifest == "" {
		t.Skip("explicit existing-Session read-only grant required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, root, baseline, turn := os.Getenv("CODEAGENT_HANDOFF_READONLY_SESSION"), os.Getenv("CODEAGENT_HANDOFF_READONLY_WORKSPACE"), os.Getenv("CODEAGENT_HANDOFF_READONLY_BASELINE"), os.Getenv("CODEAGENT_HANDOFF_READONLY_TURN")
	if id == "" || root == "" || baseline == "" || !validID(turn) {
		t.Fatal("explicit workspace, Session, initial baseline and exact Turn required")
	}
	c, _, err := NewReattachedFromManifest(ctx, manifest, root, id, baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h, err := c.Handoff(ctx, id, turn)
	if err != nil {
		t.Fatal(err)
	}
	if h.AcceptanceStatus != "not_reviewed" || h.ValidationStatus != "unknown" {
		t.Fatalf("restored process-local expectations/acceptance: %+v", h)
	}
	if h.ExecutionStatus != "completed" && h.ExecutionStatus != "cancelled" && h.ExecutionStatus != "failed" {
		t.Fatalf("nonterminal handoff: %+v", h)
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("CODEAGENT_HANDOFF_READONLY_OUTPUT"); path != "" {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write(data); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("session=%s turn=%s execution=%s validation=%s acceptance=%s commands=%d changed_files=%d diff_truncated=%v bytes=%d; send=0 approval=0 cancel=0; Go API diagnostic, not Desktop native discovery", id, turn, h.ExecutionStatus, h.ValidationStatus, h.AcceptanceStatus, len(h.Commands), len(h.ChangedFiles), h.DiffTruncated, len(data))
}
