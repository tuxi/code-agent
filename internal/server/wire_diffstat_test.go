package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/tools"
)

func wireJSON(t *testing.T, ev agent.Event) (map[string]any, string) {
	t.Helper()
	b, err := json.Marshal(toWire(ev))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out, string(b)
}

func TestTurnFinishedCarriesDiffStat(t *testing.T) {
	stat := &agent.TurnDiffStat{}
	stat.Add(tools.FileWrite{Path: "internal/agent/loop.go", Added: 12, Removed: 3, Status: tools.WriteModified})
	stat.Add(tools.FileWrite{Path: "docs/turn-diffstat-v1.md", Added: 96, Status: tools.WriteAdded})

	out, raw := wireJSON(t, agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1", DiffStat: stat})

	ds, ok := out["diffstat"].(map[string]any)
	if !ok {
		t.Fatalf("diffstat missing from %s", raw)
	}
	if ds["files_changed"] != float64(2) || ds["added_lines"] != float64(108) || ds["removed_lines"] != float64(3) {
		t.Fatalf("summary = %v, want 2 / 108 / 3", ds)
	}
	files, ok := ds["files"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %v", ds["files"])
	}
	first, _ := files[0].(map[string]any)
	// Contract §2: snake_case keys, no wrapper objects.
	for _, key := range []string{"path", "added", "removed", "status"} {
		if _, ok := first[key]; !ok {
			t.Errorf("files[0] missing %q: %v", key, first)
		}
	}
	if _, ok := first["Path"]; ok {
		t.Errorf("files[0] leaked Go field names: %v", first)
	}
}

// §2.1: a turn that wrote nothing must not ship a zeroed object — absence is
// what the client renders as "unknown".
func TestTurnFinishedOmitsEmptyDiffStat(t *testing.T) {
	_, raw := wireJSON(t, agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1"})
	if strings.Contains(raw, "diffstat") {
		t.Fatalf("empty turn shipped diffstat: %s", raw)
	}

	empty := &agent.TurnDiffStat{}
	_, raw = wireJSON(t, agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1", DiffStat: empty})
	if strings.Contains(raw, "diffstat") {
		t.Fatalf("zeroed stat shipped: %s", raw)
	}
}

// §2.3: files clips at 200, the summary keeps full-turn values.
func TestTurnFinishedCapsDiffFiles(t *testing.T) {
	stat := &agent.TurnDiffStat{}
	for i := 0; i < 250; i++ {
		stat.Add(tools.FileWrite{Path: "f" + string(rune('a'+i%26)) + strings.Repeat("x", i%9+1), Added: 1, Status: tools.WriteModified})
	}
	out, _ := wireJSON(t, agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1", DiffStat: stat})
	ds := out["diffstat"].(map[string]any)
	if got := len(ds["files"].([]any)); got != maxWireDiffFiles {
		t.Fatalf("files = %d, want %d", got, maxWireDiffFiles)
	}
	if ds["files_changed"] != float64(len(stat.Files)) {
		t.Fatalf("files_changed = %v, want the full %d", ds["files_changed"], len(stat.Files))
	}
}

// The published example must stay decodable by this package: if the struct and
// the doc drift, this fails before a client author hits it.
func TestContractExampleDecodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "turn-diffstat-example.json"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var frame wireEvent
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	if frame.Kind != string(agent.EventTurnFinished) {
		t.Fatalf("kind = %q", frame.Kind)
	}
	if frame.DiffStat == nil || len(frame.DiffStat.Files) != 3 {
		t.Fatalf("diffstat = %+v", frame.DiffStat)
	}
	if frame.DiffStat.FilesChanged != 3 || frame.DiffStat.AddedLines != 122 || frame.DiffStat.RemovedLines != 3 {
		t.Fatalf("summary = %+v", frame.DiffStat)
	}
}

// Failed turns are terminal too: they carry what was already written before the
// turn stopped.
func TestFailedTurnCarriesDiffStat(t *testing.T) {
	stat := &agent.TurnDiffStat{}
	stat.Add(tools.FileWrite{Path: "a.go", Added: 1, Status: tools.WriteModified})
	out, _ := wireJSON(t, agent.Event{Kind: agent.EventTurnFailed, TurnID: "t1", Err: "boom", DiffStat: stat})
	if _, ok := out["diffstat"]; !ok {
		t.Fatal("turn_failed dropped its diffstat")
	}
}
