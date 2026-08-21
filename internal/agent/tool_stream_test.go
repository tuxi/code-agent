package agent

import (
	"strings"
	"sync"
	"testing"
)

// under-budget chunks are persisted verbatim and no marker is produced.
func TestToolStreamCapperUnderBudget(t *testing.T) {
	c := NewToolStreamCapper()
	for i := 0; i < 10; i++ {
		chunk := strings.Repeat("a", 1000)
		if !c.NoteChunk("call1", EventToolStdout, chunk) {
			t.Fatalf("chunk %d within budget should persist", i)
		}
	}
	if ms := c.FlushCall("call1"); len(ms) != 0 {
		t.Fatalf("under-budget call should produce no marker, got %d", len(ms))
	}
}

// once a call crosses the budget, overflow chunks are dropped from persistence
// and a single marker with the tail is produced at flush.
func TestToolStreamCapperTruncatesOverflow(t *testing.T) {
	c := NewToolStreamCapper()
	// Feed 2x the budget in 8KB chunks.
	persisted, dropped := 0, 0
	chunk := strings.Repeat("x", 8192)
	for i := 0; i < 2*ToolStreamBudget/8192; i++ {
		if c.NoteChunk("call1", EventToolStdout, chunk) {
			persisted += len(chunk)
		} else {
			dropped += len(chunk)
		}
	}
	if persisted > ToolStreamBudget {
		t.Fatalf("persisted %d exceeds budget %d", persisted, ToolStreamBudget)
	}
	if dropped == 0 {
		t.Fatal("expected some chunks to be dropped from persistence")
	}

	ms := c.FlushCall("call1")
	if len(ms) != 1 {
		t.Fatalf("expected 1 marker, got %d", len(ms))
	}
	m := ms[0]
	if m.Kind != EventToolStdout {
		t.Fatalf("marker kind = %s, want tool_stdout", m.Kind)
	}
	if !strings.Contains(m.Chunk, "truncated") {
		t.Fatalf("marker should explain truncation: %q", m.Chunk)
	}
	// The marker carries the tail: last ToolStreamTailBytes of the stream.
	if !strings.Contains(m.Chunk, strings.Repeat("x", 4096)) {
		t.Fatalf("marker should include the tail")
	}
	if len(m.Chunk) > ToolStreamTailBytes+len("…[tool output truncated:  bytes persisted,  bytes dropped, final  bytes shown below]…\n")+64 {
		t.Fatalf("marker too large: %d bytes", len(m.Chunk))
	}
}

// stdout and stderr are budgeted independently within one call.
func TestToolStreamCapperSeparatesStreams(t *testing.T) {
	c := NewToolStreamCapper()
	// Over-budget stderr, under-budget stdout.
	for i := 0; i < 3*ToolStreamBudget/4096; i++ {
		c.NoteChunk("call1", EventToolStderr, strings.Repeat("e", 4096))
	}
	if !c.NoteChunk("call1", EventToolStdout, "hello") {
		t.Fatal("stdout chunk should persist (independent budget)")
	}
	ms := c.FlushCall("call1")
	if len(ms) != 1 {
		t.Fatalf("expected only the stderr marker, got %d", len(ms))
	}
	if ms[0].Kind != EventToolStderr {
		t.Fatalf("marker kind = %s, want tool_stderr", ms[0].Kind)
	}
}

// FlushCall only closes the named call; others remain tracked.
func TestToolStreamCapperFlushIsPerCall(t *testing.T) {
	c := NewToolStreamCapper()
	c.NoteChunk("call1", EventToolStdout, strings.Repeat("a", ToolStreamBudget+1))
	c.NoteChunk("call2", EventToolStdout, strings.Repeat("b", ToolStreamBudget+1))
	if ms := c.FlushCall("call1"); len(ms) != 1 {
		t.Fatalf("call1 should flush 1 marker, got %d", len(ms))
	}
	if ms := c.FlushAll(); len(ms) != 1 {
		t.Fatalf("call2 should flush 1 marker, got %d", len(ms))
	}
}

// concurrent note-takers are safe and never exceed the budget.
func TestToolStreamCapperConcurrent(t *testing.T) {
	c := NewToolStreamCapper()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			callID := "call" + string(rune('a'+g))
			for i := 0; i < 2*ToolStreamBudget/2048; i++ {
				c.NoteChunk(callID, EventToolStdout, strings.Repeat("z", 2048))
			}
		}(g)
	}
	wg.Wait()
	if ms := c.FlushAll(); len(ms) != 8 {
		t.Fatalf("expected 8 markers, got %d", len(ms))
	}
}
