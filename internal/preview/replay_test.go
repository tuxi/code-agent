package preview

import (
	"os"
	"path/filepath"
	"testing"
)

func fixtureEvent(t *testing.T, name string) Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1", name))
	if err != nil {
		t.Fatal(err)
	}
	event, err := DecodeEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestReplayNeverLetsLowerRevisionOverwriteHigher(t *testing.T) {
	revisionTwo := fixtureEvent(t, "event-revision-2.json")
	revisionOne := fixtureEvent(t, "event-revision-1.json")
	result := ApplyReplay([]Event{revisionTwo, revisionOne})
	if result.Error != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Revision != 2 {
		t.Fatalf("replay result = %+v", result)
	}
}

func TestReplayDuplicateRevisionIsIdempotent(t *testing.T) {
	first := fixtureEvent(t, "event-revision-1.json")
	duplicate := fixtureEvent(t, "event-duplicate.json")
	result := ApplyReplay([]Event{first, duplicate})
	if result.Error != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Revision != 1 {
		t.Fatalf("duplicate replay result = %+v", result)
	}
}

func TestReplayRejectsEventEnvelopeMismatch(t *testing.T) {
	event := fixtureEvent(t, "event-revision-1.json")
	event.SessionID = "other_session"
	result := ApplyReplay([]Event{event})
	if result.Error == nil || result.Error.Code != CodeInvalidArtifact {
		t.Fatalf("mismatch replay result = %+v", result)
	}
}
