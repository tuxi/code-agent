//go:build unix

package collabflux_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/collab"
)

// Explicit offline export for the Go/Swift UI contract; never uses the user's Daemon.
func TestExportManagedTimelineFixture(t *testing.T) {
	out := os.Getenv("CODEAGENT_TASK_FIXTURE_OUT")
	if out == "" {
		t.Skip("set CODEAGENT_TASK_FIXTURE_OUT to export the offline UI fixture")
	}
	f := newProcessFixture(t, "real_loop")
	f.setupTask()
	var receipt collab.Receipt
	f.must("codeagent_start_task_run", map[string]any{"task_id": f.taskID, "run": f.run}, &receipt)
	first := f.waitReview(1)
	if err := f.review(first, "request_changes"); err != nil {
		t.Fatal(err)
	}
	second := f.waitReview(2)
	if err := f.review(second, "pass"); err != nil {
		t.Fatal(err)
	}
	f.wait(func() bool { return f.snapshot().TaskAcceptance == "accepted" })
	snapshot := f.snapshot()
	var page collab.EventPage
	f.must("codeagent_read_task_events", map[string]any{"task_id": f.taskID, "after": "0", "limit": 200}, &page)
	if page.HasMore || f.effects() != 2 {
		t.Fatal("incomplete fixture", page.HasMore, f.effects())
	}
	bundles := map[string]collab.ReviewBundle{}
	blobs := map[string][]byte{}
	// Preserve original bytes: MCP JSON decoding may reorder objects inside
	// RawMessage fields, so the API DTO cannot reproduce the stored blob hash.
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "sessions.db")+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ref := range snapshot.ArtifactRefs {
		if ref.Kind == "review_bundle" {
			var bundle collab.ReviewBundle
			f.must("codeagent_read_review_bundle", map[string]any{"task_id": f.taskID, "artifact_id": ref.ArtifactID}, &bundle)
			bundles[ref.ArtifactID] = bundle
			var raw []byte
			if err := db.QueryRow(`SELECT document FROM coding_task_artifact_blobs WHERE id=? AND task_id=?`, ref.ArtifactID, snapshot.TaskID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if ref.ContentHash == nil || collab.PayloadHash(raw) != *ref.ContentHash {
				t.Fatal("original blob hash mismatch")
			}
			blobs[ref.ArtifactID] = raw
		}
	}
	if len(bundles) != 2 {
		t.Fatal("missing frozen rounds")
	}
	data, err := json.MarshalIndent(map[string]any{"fixture_only": true, "provenance": "offline actual Agent Loop + SQLite + Flux + MCP, two edits, request_changes/pass/finalize", "snapshot": snapshot, "event_page": page, "bundles": bundles, "bundle_blobs": blobs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(out) {
		t.Fatal("export requires an explicit absolute output file")
	}
	if err = os.WriteFile(out, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
