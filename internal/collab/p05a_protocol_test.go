package collab

import (
	"encoding/json"
	"os"
	"testing"
)

func TestP05ManagedTimelineSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../docs/contracts/p05a-managed-task-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Snapshot Snapshot                `json:"snapshot"`
		Page     EventPage               `json:"event_page"`
		Bundles  map[string]ReviewBundle `json:"bundles"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Snapshot.TaskAcceptance != "accepted" || f.Page.HasMore || f.Page.After != "0" || f.Page.NextCursor != f.Snapshot.SnapshotTaskSeq || len(f.Bundles) != 2 {
		t.Fatal("incomplete managed timeline fixture")
	}
	verdicts := map[string]bool{}
	for _, e := range f.Page.Events {
		if e.TaskID != f.Snapshot.TaskID {
			t.Fatal("cross-task event")
		}
		if e.Kind == "review.recorded" {
			var decision GateDecision
			if err := json.Unmarshal(e.Payload, &decision); err != nil {
				t.Fatal(err)
			}
			verdicts[decision.Review.Verdict] = true
			b, ok := f.Bundles[decision.Review.Artifact.ArtifactID]
			if !ok || b.AttemptID != decision.Review.AttemptID || b.Plan != decision.Review.Plan || b.Source.RunID != decision.Review.RunID || b.Source.Epoch != decision.Review.Epoch {
				t.Fatal("review does not bind its historical candidate")
			}
		}
	}
	if !verdicts["pass"] || !verdicts["request_changes"] {
		t.Fatal("missing repair history")
	}
	for _, a := range f.Snapshot.ArtifactRefs {
		if a.Kind != "review_bundle" {
			continue
		}
		b := f.Bundles[a.ArtifactID]
		if b.AttemptID != a.AttemptID || b.TurnID != a.SourceRef.TurnID || b.EventCursor != a.SourceRef.Seq || a.ContentHash == nil || a.CaptureMode != "immutable" {
			t.Fatal("broken frozen artifact binding")
		}
	}
}
