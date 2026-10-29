//go:build unix

package mcpbridge

import (
	"context"
	"os"
	"testing"
	"time"
)

// Explicitly opt-in and read-only. This exercises the user's existing daemon,
// never a model provider or input/approval operation. Do not enable in CI.
func TestExistingDaemonReadOnlySocketReconnection(t *testing.T) {
	manifest := os.Getenv("CODEAGENT_GUI_READONLY_MANIFEST")
	if manifest == "" {
		t.Skip("explicit local GUI read-only grant required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workspace, id, baseline, turn := os.Getenv("CODEAGENT_GUI_READONLY_WORKSPACE"), os.Getenv("CODEAGENT_GUI_READONLY_SESSION"), os.Getenv("CODEAGENT_GUI_READONLY_BASELINE"), os.Getenv("CODEAGENT_GUI_READONLY_TURN")
	if workspace == "" || id == "" || baseline == "" || !validID(turn) {
		t.Fatal("explicit workspace/session/baseline/cancelled Turn are required")
	}
	c, info, err := NewReattachedFromManifest(ctx, manifest, workspace, id, baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	before, err := c.Read(ctx, c.id, turn, 0)
	if err != nil || before.Status != "cancelled" || !before.StreamConnected {
		t.Fatalf("before: %+v %v", before, err)
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	// Sever only this newly created Bridge's own real WSS connection. The
	// Talkify channel, daemon, network settings and other sessions are untouched.
	_ = conn.CloseNow()
	for {
		c.mu.Lock()
		disconnected := c.conn == nil
		c.mu.Unlock()
		if disconnected {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	gap, err := c.Read(ctx, c.id, turn, before.Cursor)
	if err != nil || gap.Status != before.Status {
		t.Fatalf("gap: %+v %v", gap, err)
	}
	// Public Read performs a bounded observation-only reconnect after backoff.
	var after ReadResult
	for {
		after, err = c.Read(ctx, c.id, turn, before.Cursor)
		if err != nil || after.StreamConnected {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err != nil || after.Status != before.Status || !after.StreamConnected || after.Cursor != before.Cursor {
		t.Fatalf("after: %+v %v", after, err)
	}
	t.Logf("verified server=%s store=%s session=%s cursor=%d status=%s; own WSS severed then reconnected; no input/approval sent", info.ServerID, info.StoreID, c.id, after.Cursor, after.Status)
}
