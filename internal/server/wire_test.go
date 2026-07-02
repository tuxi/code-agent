package server

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/tools"
)

var update = flag.Bool("update", false, "regenerate golden files")

// fixedAt is a deterministic timestamp so golden frames are stable across runs.
var fixedAt = time.Date(2026, 6, 24, 10, 0, 0, 123000000, time.UTC)

type wireCase struct {
	ev     agent.Event
	parent string
}

// cases cover every interesting shape: structured tool_args, the
// duration->elapsed_ms conversion, a nested struct ([]Todo), the compaction
// numerics, and a subagent event carrying parent_session_id.
func cases() map[string]wireCase {
	return map[string]wireCase{
		"tool_started": {ev: agent.Event{
			Kind: agent.EventToolStarted, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7", CallID: "call_abc", Step: 3,
			ToolName: "run_command", ToolArgs: `{"command":"go test ./..."}`,
		}},
		"tool_started_client": {ev: agent.Event{
			Kind: agent.EventToolStarted, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7", CallID: "call_99", Step: 4,
			ToolName: "trim_video", ToolArgs: `{"url":"file:///tmp/input.mp4","start":0,"duration":10}`,
			Executor: "client",
		}},
		"tool_finished": {ev: agent.Event{
			Kind: agent.EventToolFinished, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7", CallID: "call_abc", Step: 3,
			ToolName: "run_command", Observation: `{"command":"go test ./...","stdout":"ok  ./internal/agent","exit_code":0,"duration_ms":234}`,
			Elapsed: 1200 * time.Millisecond,
		}},
		"tool_finished_assets": {ev: agent.Event{
			Kind: agent.EventToolFinished, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7", CallID: "call_grep", Step: 4,
			ToolName:    "grep",
			Observation: `Sources/App.swift:49: public var streamingText: String = ""`,
			Output:      json.RawMessage(`{"kind":"search_results","query":"streamingText","items":[{"asset_id":"asset_turn_7_call_grep_001_7156f5c8","kind":"file_location","path":"Sources/App.swift","line":49,"column":1,"preview":"public var streamingText: String = \"\""}]}`),
			Assets: []assets.Ref{{
				ID:                    "asset_turn_7_call_grep_001_7156f5c8",
				Kind:                  "file_location",
				URI:                   "workspace://app-local/Sources/App.swift#L49C1",
				DisplayName:           "App.swift:49",
				WorkspaceID:           "app-local",
				WorkspaceRelativePath: "Sources/App.swift",
				AbsolutePath:          "/work/App/Sources/App.swift",
				Range:                 &assets.Range{StartLine: 49, StartColumn: 1},
				Preview:               `public var streamingText: String = ""`,
				MIMEType:              "text/x-swift",
				Metadata:              map[string]string{"language": "swift"},
				SourceTurnID:          "turn_7",
				SourceCallID:          "call_grep",
			}},
			Elapsed: 1200 * time.Millisecond,
		}},
		"tool_stdout": {ev: agent.Event{
			Kind: agent.EventToolStdout, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7", CallID: "call_abc",
			Chunk: "Downloading packages...\n",
		}},
		"turn_finished_annotations": {ev: agent.Event{
			Kind: agent.EventTurnFinished, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			Text: "Open `App.swift:5` for the important line.",
			TextAnnotations: []assets.TextAnnotation{{
				AssetID:      "asset_turn_7_call_grep_001_7156f5c8",
				Kind:         "file_location",
				Text:         "App.swift:5",
				StartByte:    6,
				EndByte:      17,
				StartUTF16:   6,
				EndUTF16:     17,
				SourceTurnID: "turn_7",
				SourceCallID: "call_grep",
			}},
		}},
		"tool_stderr": {ev: agent.Event{
			Kind: agent.EventToolStderr, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7", CallID: "call_abc",
			Chunk: "Warning: deprecated package\n",
		}},
		"model_finished": {ev: agent.Event{
			Kind: agent.EventModelFinished, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			PromptTokens: 4096, Elapsed: 731 * time.Millisecond,
		}},
		"todo_updated": {ev: agent.Event{
			Kind: agent.EventTodoUpdated, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			Todos: []tools.Todo{
				{Content: "write wire.go", ActiveForm: "writing wire.go", Status: tools.TodoInProgress},
				{Content: "add golden tests", Status: tools.TodoPending},
			},
		}},
		"compacted": {ev: agent.Event{
			Kind: agent.EventCompacted, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			BeforeTokens: 90000, AfterTokens: 30000, SavedTokens: 60000,
			SummaryChars: 1200, Ratio: 0.33,
		}},
		"task_started": {ev: agent.Event{
			Kind: agent.EventTaskStarted, At: fixedAt,
			SessionID: "sess_child", TurnID: "turn_7",
			Text: "investigate the auth module",
		}, parent: "sess_root"},
		"plan_proposed": {ev: agent.Event{
			Kind: agent.EventPlanProposed, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			Text: "# Implementation Plan\n\n1. Add login\n2. Add middleware",
		}},
		"plan_approved": {ev: agent.Event{
			Kind: agent.EventPlanApproved, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			Text: "plan_abc123",
		}},
		"plan_rejected": {ev: agent.Event{
			Kind: agent.EventPlanRejected, At: fixedAt,
			SessionID: "sess_root", TurnID: "turn_7",
			Text: "plan_abc123",
		}},
	}
}

// TestGolden locks Protocol v1: each frame must byte-match its golden file. Run
// `go test ./internal/server -run Golden -update` to regenerate after an
// intentional contract change — CI then diffs any unintended field change.
func TestGolden(t *testing.T) {
	for name, c := range cases() {
		t.Run(name, func(t *testing.T) {
			frame, err := Encode(c.ev, "evt_fixed", c.parent)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, frame, "", "  "); err != nil {
				t.Fatalf("indent: %v", err)
			}
			pretty.WriteByte('\n')
			path := filepath.Join("testdata", name+".json")

			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(pretty.Bytes(), want) {
				t.Errorf("frame mismatch for %s\n got:\n%s\nwant:\n%s", name, pretty.Bytes(), want)
			}
		})
	}
}

// TestWireRoundTrip proves the json tags are symmetric: a frame survives
// marshal -> unmarshal -> marshal unchanged, so no field is lost or renamed.
func TestWireRoundTrip(t *testing.T) {
	for name, c := range cases() {
		t.Run(name, func(t *testing.T) {
			b1, err := Encode(c.ev, "evt_x", c.parent)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var w wireEvent
			if err := json.Unmarshal(b1, &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			b2, err := json.Marshal(w)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if !bytes.Equal(b1, b2) {
				t.Errorf("round-trip not stable\n first: %s\nsecond: %s", b1, b2)
			}
		})
	}
}

// TestElapsedIsMilliseconds guards the most dangerous wire decision: durations
// must go out as ms, never Go's default ns.
func TestElapsedIsMilliseconds(t *testing.T) {
	frame, err := Encode(agent.Event{Kind: agent.EventModelFinished, At: fixedAt, Elapsed: 731 * time.Millisecond}, "id", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame, []byte(`"elapsed_ms":731`)) {
		t.Errorf("want elapsed_ms:731 (ms, not ns), got %s", frame)
	}
}

// TestToolArgsAreStructured guards the second dangerous decision: tool_args is an
// embedded JSON object, and stays valid JSON even when the args are not JSON.
func TestToolArgsAreStructured(t *testing.T) {
	frame, err := Encode(agent.Event{Kind: agent.EventToolStarted, At: fixedAt, ToolArgs: `{"command":"git push"}`}, "id", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame, []byte(`"tool_args":{"command":"git push"}`)) {
		t.Errorf("tool_args must be an embedded JSON object, got %s", frame)
	}
	frame2, err := Encode(agent.Event{Kind: agent.EventToolStarted, At: fixedAt, ToolArgs: "not json"}, "id", "")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(frame2) {
		t.Errorf("frame must stay valid JSON for non-JSON args, got %s", frame2)
	}
}

// bufSink captures emitted frames for assertions.
type bufSink struct{ frames [][]byte }

func (b *bufSink) Send(f []byte) error {
	b.frames = append(b.frames, append([]byte(nil), f...))
	return nil
}

// TestStreamEmitter proves StreamEmitter is a drop-in agent.Emitter and that the
// emitter (not toWire) stamps event_id and parent_session_id.
func TestStreamEmitter(t *testing.T) {
	sink := &bufSink{}
	em := NewStreamEmitter(sink).WithParent("sess_root")
	em.newID = func() string { return "evt_fixed" }

	em.Emit(agent.Event{Kind: agent.EventTaskStarted, At: fixedAt, SessionID: "sess_child", Text: "go"})

	if len(sink.frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(sink.frames))
	}
	f := sink.frames[0]
	for _, want := range []string{
		`"event_id":"evt_fixed"`,
		`"kind":"task_started"`,
		`"session_id":"sess_child"`,
		`"parent_session_id":"sess_root"`,
	} {
		if !bytes.Contains(f, []byte(want)) {
			t.Errorf("frame missing %s\ngot: %s", want, f)
		}
	}
}

func TestHelloPinsVersion(t *testing.T) {
	frame, err := Hello("codeagent/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame, []byte(`"protocol_version":1`)) || !bytes.Contains(frame, []byte(`"type":"hello"`)) {
		t.Errorf("hello frame wrong: %s", frame)
	}
}
