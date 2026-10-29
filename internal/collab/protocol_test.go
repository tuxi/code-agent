package collab

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

func TestB1SharedFixtureAndUnknownEvent(t *testing.T) {
	raw, err := os.ReadFile("../../docs/contracts/p0b-task-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Snapshot Snapshot  `json:"snapshot"`
		Page     EventPage `json:"event_page"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	s, p := fixture.Snapshot, fixture.Page
	if s.Schema != "task-snapshot/v1" || p.Schema != "task-event-page/v1" || s.TaskID != p.TaskID || p.NextCursor != s.SnapshotTaskSeq || s.WorkflowRef != nil || s.CostUSD != nil || s.Usage != nil {
		t.Fatal("fixture envelope invariant")
	}
	parts := map[string]TaskParticipation{}
	for _, part := range s.Participations {
		if part.TaskID != s.TaskID {
			t.Fatal("wrong task")
		}
		parts[part.ParticipationID] = part
	}
	if parts["part_plan"].SessionRef.SessionHandle != parts["part_review"].SessionRef.SessionHandle || parts["part_plan"].Role == parts["part_review"].Role {
		t.Fatal("session/role conflation")
	}
	attempts := map[string]TaskAttempt{}
	for _, a := range s.Attempts {
		if _, ok := parts[a.ParticipationID]; !ok {
			t.Fatal("missing participation")
		}
		attempts[a.AttemptID] = a
	}
	for _, a := range s.ArtifactRefs {
		if _, ok := attempts[a.AttemptID]; !ok {
			t.Fatal("missing attempt")
		}
		if a.CaptureMode == "immutable_snapshot" {
			t.Fatal("B1 fixture claims immutable capture")
		}
	}
	for _, e := range p.Events {
		if _, ok := attempts[e.AttemptID]; !ok {
			t.Fatal("missing source attempt")
		}
		if _, err = strconv.ParseInt(e.TaskSeq, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	// All identifiers/cursors remain decimal strings beyond JavaScript's exact
	// integer range. Unknown kinds and payloads survive a Go/Swift round trip.
	e := p.Events[0]
	e.Kind = "future.progress"
	e.TaskSeq = "9007199254740993"
	e.Payload = json.RawMessage(`{"future":{"phase":"new"}}`)
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TaskEvent
	if err = json.Unmarshal(data, &decoded); err != nil || decoded.Kind != e.Kind || decoded.TaskSeq != e.TaskSeq || string(decoded.Payload) != string(e.Payload) {
		t.Fatalf("roundtrip=%s %v", data, err)
	}
}
