package agent

import (
	"fmt"
	"strings"
	"sync"
)

// Tool stream persistence budget (P1): a single tool call's stdout/stderr is
// streamed chunk-by-chunk as tool_stdout/tool_stderr events, and the persistence
// layer used to store every chunk verbatim — an xcodebuild run could persist
// hundreds of MB into session_events (observed: 342MB from one call), which then
// blew up replay memory and got the daemon jetsam-killed.
//
// The fix caps persistence PER TOOL CALL: the first ToolStreamBudget bytes are
// stored verbatim (head), the overflow is dropped from persistence, but the last
// ToolStreamTailBytes are retained in memory and written once as a marker event
// when the call finishes — replay shows the beginning AND the end of a long
// output, not hundreds of MB of it. Live streaming is untouched: the loop still
// fans every chunk to connected subscribers; the cap only decides what lands in
// the event store.
const (
	// ToolStreamBudget is the head budget: how many bytes of one call's stream
	// are persisted verbatim before truncation kicks in.
	ToolStreamBudget = 64 * 1024
	// ToolStreamTailBytes is how much of the overflow tail is retained and
	// written in the marker event at tool_finished.
	ToolStreamTailBytes = 4 * 1024
)

// ToolStreamCapper tracks per-call tool stream bytes for the persistence layer.
// It is keyed by stream kind (tool_stdout vs tool_stderr) within a call, and is
// safe for concurrent use — the loop may run tool batches in parallel, and the
// daemon multiplexes sessions.
type ToolStreamCapper struct {
	mu    sync.Mutex
	calls map[string]*toolStreamState
}

type toolStreamState struct {
	// seen is total bytes across the whole stream (all chunks).
	seen int64
	// persisted is bytes stored verbatim so far (head budget spent).
	persisted int64
	// tail is a rolling buffer holding the last ToolStreamTailBytes of overflow.
	tail []byte
	// dropped counts overflow bytes not persisted (beyond head).
	dropped int64
	// over is set once the stream crosses ToolStreamBudget.
	over bool
}

// NewToolStreamCapper returns an empty capper.
func NewToolStreamCapper() *ToolStreamCapper {
	return &ToolStreamCapper{calls: make(map[string]*toolStreamState)}
}

// streamKey combines a call identity and stream kind into one map key. kind is
// EventToolStdout or EventToolStderr; other values share a single bucket.
func streamKey(callID string, kind EventKind) string {
	return callID + "\x00" + string(kind)
}

// NoteChunk records one chunk of a call's stream. It reports whether the chunk
// should be PERSISTED verbatim (true) or is overflow that the caller must drop
// from persistence but may still fan out live (false).
func (c *ToolStreamCapper) NoteChunk(callID string, kind EventKind, chunk string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := streamKey(callID, kind)
	st := c.calls[key]
	if st == nil {
		st = &toolStreamState{}
		c.calls[key] = st
	}
	n := int64(len(chunk))
	st.seen += n
	if !st.over && st.persisted+n <= ToolStreamBudget {
		st.persisted += n
		return true
	}
	st.over = true
	if n > 0 {
		st.dropped += n
		st.tail = appendTail(st.tail, []byte(chunk), ToolStreamTailBytes)
	}
	return false
}

// FlushCall closes a call's streams and returns the marker events the
// persistence layer should write (usually at tool_finished). Markers are
// summaries — persist them, but do NOT fan them to live subscribers, who already
// received the full stream. A nil/empty result means the call stayed within
// budget and needs no marker. The call's state is removed.
func (c *ToolStreamCapper) FlushCall(callID string) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	prefix := callID + "\x00"
	for key, st := range c.calls {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		delete(c.calls, key)
		kind := EventKind(key[len(prefix):])
		out = append(out, st.markers(kind)...)
	}
	return out
}

// FlushAll closes every open stream and returns their markers (used at turn
// end so a tool that died without tool_finished still leaves a bounded record).
// State is cleared.
func (c *ToolStreamCapper) FlushAll() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for key, st := range c.calls {
		delete(c.calls, key)
		kind := EventKind(key[stringsIndexByte(key, 0)+1:])
		out = append(out, st.markers(kind)...)
	}
	return out
}

// markers builds the marker event(s) for a closed stream, if it was truncated.
func (s *toolStreamState) markers(kind EventKind) []Event {
	if !s.over {
		return nil
	}
	return []Event{{
		Kind:  kind,
		Chunk: fmt.Sprintf("\n…[tool output truncated: %d bytes persisted, %d bytes dropped, final %d bytes shown below]…\n%s",
			s.persisted, s.dropped, len(s.tail), s.tail),
	}}
}

// stringsIndexByte finds the first NUL in s (the streamKey separator).
func stringsIndexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// appendTail keeps buf to at most max bytes by dropping from the front, without
// splitting a UTF-8 rune.
func appendTail(buf, p []byte, max int) []byte {
	buf = append(buf, p...)
	if len(buf) <= max {
		return buf
	}
	drop := len(buf) - max
	for drop < len(buf) && (buf[drop]&0xC0) == 0x80 {
		drop++
	}
	return append([]byte(nil), buf[drop:]...)
}
