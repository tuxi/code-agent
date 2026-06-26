package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
)

// chanSubscriber is a fake Subscriber the test drives directly.
type chanSubscriber struct {
	ch       chan agent.Event
	unsubbed bool
}

func (s *chanSubscriber) Subscribe() (<-chan agent.Event, func()) {
	return s.ch, func() { s.unsubbed = true }
}

// errSink returns an error on the failAt-th Send (1-based).
type errSink struct {
	failAt int
	n      int
}

func (s *errSink) Send([]byte) error {
	s.n++
	if s.n >= s.failAt {
		return errors.New("client gone")
	}
	return nil
}

// syncSink is a concurrency-safe FrameSink for the goroutine-driven tests.
type syncSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *syncSink) Send(f []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, append([]byte(nil), f...))
	return nil
}

func (s *syncSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func (s *syncSink) at(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.frames[i]...)
}

func waitForCount(t *testing.T, s *syncSink, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.count() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frames (have %d)", n, s.count())
}

func TestBridgeStreamsHelloThenEvents(t *testing.T) {
	sub := &chanSubscriber{ch: make(chan agent.Event, 8)}
	sink := &bufSink{}
	b := NewBridge(sink)

	sub.ch <- agent.Event{Kind: agent.EventThinking, Text: "hi"}
	close(sub.ch) // bridge drains the buffered event, then sees closed and returns

	if err := b.Run(context.Background(), sub, "codeagent/test"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.frames) != 2 {
		t.Fatalf("want hello + 1 event frame, got %d", len(sink.frames))
	}

	var hello map[string]any
	if err := json.Unmarshal(sink.frames[0], &hello); err != nil {
		t.Fatal(err)
	}
	if hello["type"] != "hello" || hello["protocol_version"].(float64) != 1 {
		t.Errorf("first frame is not the hello handshake: %s", sink.frames[0])
	}

	var ev map[string]any
	if err := json.Unmarshal(sink.frames[1], &ev); err != nil {
		t.Fatal(err)
	}
	if ev["kind"] != "thinking" || ev["text"] != "hi" {
		t.Errorf("event frame wrong: %s", sink.frames[1])
	}
	if !sub.unsubbed {
		t.Error("bridge did not unsubscribe on return")
	}
}

func TestBridgeStopsAndUnsubscribesOnSinkError(t *testing.T) {
	sub := &chanSubscriber{ch: make(chan agent.Event, 8)}
	sub.ch <- agent.Event{Kind: agent.EventThinking}
	b := NewBridge(&errSink{failAt: 2}) // hello ok, first event Send fails

	if err := b.Run(context.Background(), sub, "s"); err == nil {
		t.Error("want an error when the sink fails")
	}
	if !sub.unsubbed {
		t.Error("bridge must unsubscribe even on a sink error")
	}
}

func TestBridgeStopsOnContextCancel(t *testing.T) {
	sub := &chanSubscriber{ch: make(chan agent.Event)} // never delivers
	b := NewBridge(&bufSink{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, sub, "s") }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancel")
	}
	if !sub.unsubbed {
		t.Error("bridge must unsubscribe on cancel")
	}
}

// TestBridgeEndToEndFromRealConversation is the weld proof: a real Conversation
// (whose hub conversation.New installed as the Runner's Emitter) -> Subscribe ->
// Bridge -> Encode -> a JSON frame on the wire, with the v1 contract intact
// (structured tool_args, stamped event_id).
func TestBridgeEndToEndFromHub(t *testing.T) {
	hub := newTestHub()
	sess := &testSession{hub: hub}

	sink := &syncSink{}
	b := NewBridge(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, sess, "codeagent/test") }()

	waitForCount(t, sink, 1) // hello sent => the bridge has subscribed

	// Emit a core event through the hub — as the agent loop would
	// through SessionEventBus.
	hub.Emit(agent.Event{
		Kind: agent.EventToolStarted, SessionID: "sess_root", TurnID: "turn_1",
		Step: 1, ToolName: "run_command", ToolArgs: `{"command":"go test ./..."}`,
	})
	waitForCount(t, sink, 2)

	var ev map[string]any
	if err := json.Unmarshal(sink.at(1), &ev); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	if ev["kind"] != "tool_started" || ev["tool_name"] != "run_command" {
		t.Errorf("unexpected frame: %s", sink.at(1))
	}
	args, ok := ev["tool_args"].(map[string]any)
	if !ok || args["command"] != "go test ./..." {
		t.Errorf("tool_args must be a structured JSON object: %s", sink.at(1))
	}
	if id, _ := ev["event_id"].(string); id == "" {
		t.Errorf("event_id was not stamped: %s", sink.at(1))
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}
