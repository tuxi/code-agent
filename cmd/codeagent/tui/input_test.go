package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// mouseProbe is a minimal tea.Model that records every message it receives.
type mouseProbe struct {
	got []tea.Msg
}

func (m *mouseProbe) Init() tea.Cmd { return nil }
func (m *mouseProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.got = append(m.got, msg)
	return m, nil
}
func (m *mouseProbe) View() tea.View {
	v := tea.NewView("")
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// readAllFilter drains the filter until EOF and returns the filtered bytes.
func readAllFilter(t *testing.T, f *sgrMouseFilter) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 64)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("filter read: %v", err)
		}
	}
}

func newPipeFilter(t *testing.T, input string) (*sgrMouseFilter, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()
	return newSGRMouseFilter(r), w
}

// Some terminals emit SGR mouse sequences with spaces inside them; bubbletea's
// parser only accepts the canonical no-space form. The filter must produce the
// canonical bytes so the parser delivers a MouseMsg instead of KeyRunes.
func TestSGRFilterStripSpacesInSequence(t *testing.T) {
	f, _ := newPipeFilter(t, "\x1b[<64; 58; 6M")
	got := readAllFilter(t, f)
	want := "\x1b[<64;58;6M"
	if string(got) != want {
		t.Fatalf("filtered = %q, want %q", got, want)
	}
}

func TestSGRFilterStripsSpacesAcrossAllFields(t *testing.T) {
	f, _ := newPipeFilter(t, "\x1b[<65 ; 40 ; 12 m")
	got := readAllFilter(t, f)
	if string(got) != "\x1b[<65;40;12m" {
		t.Fatalf("filtered = %q, want %q", got, "\x1b[<65;40;12m")
	}
}

// A standard no-space sequence must pass through unchanged.
func TestSGRFilterLeavesCanonicalSequenceUntouched(t *testing.T) {
	f, _ := newPipeFilter(t, "\x1b[<64;58;6M")
	got := readAllFilter(t, f)
	if string(got) != "\x1b[<64;58;6M" {
		t.Fatalf("filtered = %q, want the canonical sequence unchanged", got)
	}
}

// Normal keyboard input must never be altered — including literal spaces and
// text that merely contains '[' and '<'.
func TestSGRFilterPreservesNormalInput(t *testing.T) {
	in := "hello world [a< b> \x1b[1;5D ctrl-left"
	f, _ := newPipeFilter(t, in)
	got := readAllFilter(t, f)
	if string(got) != in {
		t.Fatalf("filtered = %q, want input unchanged %q", got, in)
	}
}

// A sequence split across reads (escape prefix in one read, body in the next)
// must still be normalized across the boundary.
func TestSGRFilterHandlesSplitSequence(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	f := newSGRMouseFilter(r)

	// Split mid-sequence: "ESC[<64;" in the first write, " 58; 6M" in the next.
	if _, err := io.WriteString(w, "\x1b[<64;"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	// The incomplete sequence is held back, so the first read returns nothing.
	if n != 0 {
		t.Fatalf("first read returned %q, want nothing (incomplete sequence held back)", buf[:n])
	}
	if _, err := io.WriteString(w, " 58; 6M"); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()
	n, err = f.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("second read: %v", err)
	}
	got := string(buf[:n])
	if got != "\x1b[<64;58;6M" {
		t.Fatalf("second read = %q, want %q", got, "\x1b[<64;58;6M")
	}
}

// A burst of wheel events (fast scrolling) must survive intact even when the
// read boundaries cut through sequences. This is the exact regression that
// used to garble the composer: the old filter silently dropped up to 3 bytes
// from the tail of a read that ended with an escape prefix and was followed by
// a full read.
func TestSGRFilterFastScrollBurst(t *testing.T) {
	var burst strings.Builder
	for i := 0; i < 40; i++ {
		// Alternate wheel up/down with spaces in different positions.
		btn := 64 + i%2
		fmt.Fprintf(&burst, "\x1b[<%d; %d; %dM", btn, 30+i, 5+i%10)
	}
	spaced := burst.String()

	f, _ := newPipeFilter(t, spaced)
	got := readAllFilter(t, f)

	// Expected: every sequence canonicalized (spaces removed).
	var want strings.Builder
	for i := 0; i < 40; i++ {
		btn := 64 + i%2
		fmt.Fprintf(&want, "\x1b[<%d;%d;%dM", btn, 30+i, 5+i%10)
	}
	if string(got) != want.String() {
		t.Fatalf("burst filtered to %d bytes, want %d; got %q want %q",
			len(got), want.Len(), got, want.String())
	}
}

// The precise drop scenario: the first read ends with the "\x1b[<" prefix
// (held back), and the next read returns a FULL buffer. The old filter's
// copy(p, filtered) then truncated the tail and lost bytes. The filter must
// release everything across as many reads as needed, byte-for-byte.
func TestSGRFilterDropsNothingOnFullReads(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	f := newSGRMouseFilter(r)

	// First write ends exactly at the start of a sequence.
	head := "abcdefgh\x1b[<"
	if _, err := io.WriteString(w, head); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := string(buf[:n]); got != "abcdefgh" {
		t.Fatalf("first read = %q, want %q", got, "abcdefgh")
	}
	all := append([]byte{}, buf[:n]...)

	// Second write is a FULL buffer's worth: the continuation of the held
	// sequence plus padding bytes (total 256). Combined with the 3 held-back
	// prefix bytes this exceeds one buffer, which used to truncate.
	tail := "64;58;6M" + strings.Repeat("x", 248)
	if _, err := io.WriteString(w, tail); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()

	var got []byte
	for {
		n, err := f.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	got = append(all, got...)

	want := "abcdefgh" + "\x1b[<64;58;6M" + strings.Repeat("x", 248)
	if string(got) != want {
		t.Fatalf("got %d bytes, want %d; got %q", len(got), len(want), got)
	}
	if len(got) != len(want) {
		t.Fatalf("byte count mismatch: got %d, want %d", len(got), len(want))
	}
}

// End-to-end: the filtered stream, fed into a real bubbletea program with
// mouse capture enabled, must arrive as a MouseMsg (not KeyRunes that would be
// typed into the composer).
func TestSGRFilterThroughProgram(t *testing.T) {
	probe := &mouseProbe{}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if _, err := io.WriteString(w, "\x1b[<64; 58; 6M"); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()

	p := tea.NewProgram(probe, tea.WithInput(newSGRMouseFilter(r)), tea.WithoutRenderer())
	go func() {
		time.Sleep(1500 * time.Millisecond)
		p.Quit()
	}()
	if _, err := p.Run(); err != nil {
		t.Fatalf("program run: %v", err)
	}

	// v2 sends lifecycle messages (ColorProfileMsg, WindowSizeMsg,
	// EnvironmentMsg) at startup before input is processed; find the first
	// mouse event in the stream rather than assuming it is message #1.
	var mouse tea.MouseWheelMsg
	var ok bool
	for _, m := range probe.got {
		if mouse, ok = m.(tea.MouseWheelMsg); ok {
			break
		}
	}
	if !ok {
		var got []string
		for _, m := range probe.got {
			got = append(got, fmt.Sprintf("%s %q", m, m))
		}
		t.Fatalf("no MouseWheelMsg received; all: %s", strings.Join(got, "; "))
	}
	if mouse.Button != tea.MouseWheelUp {
		t.Fatalf("button = %d, want WheelUp", mouse.Button)
	}
	if mouse.X != 57 || mouse.Y != 5 {
		t.Fatalf("coords = (%d,%d), want (57,5)", mouse.X, mouse.Y)
	}
}

// Ensure the filtered input's output is byte-identical for a mixed stream of
// real keyboard data and multiple wheel events.
func TestSGRFilterMixedStream(t *testing.T) {
	in := "abc \x1b[<64;58;6M\x1b[<65;58;6M def"
	f, _ := newPipeFilter(t, in)
	got := readAllFilter(t, f)
	if string(got) != in {
		t.Fatalf("filtered = %q, want %q", got, in)
	}
}

// The terminal pads spaces at arbitrary positions — even between '[' and '<',
// or right after '<'. The state-machine filter must strip them all.
func TestSGRFilterVariousSpacePositions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[< 64; 58; 6M", "\x1b[<64;58;6M"},
		{"\x1b[ <64; 58; 6M", "\x1b[<64;58;6M"}, // space between '[' and '<'
		{"\x1b[<64 ;58 ; 6M", "\x1b[<64;58;6M"},
		{"\x1b[< 64 ;  58  ; 6 M", "\x1b[<64;58;6M"},
		{"\x1b[<64;58;6M", "\x1b[<64;58;6M"}, // canonical untouched
	}
	for _, tc := range cases {
		f, _ := newPipeFilter(t, tc.in)
		got := readAllFilter(t, f)
		if string(got) != tc.want {
			t.Errorf("filtered %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A sequence split at ANY byte position must still be normalized: the filter
// holds back incomplete prefixes and re-attaches a split ESC when the next
// read continues with the sequence body.
func TestSGRFilterSplitAtEveryPoint(t *testing.T) {
	seq := "\x1b[< 64; 58; 6M"
	canonical := "\x1b[<64;58;6M"
	for k := 1; k < len(seq); k++ {
		t.Run(fmt.Sprintf("split_%d", k), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			t.Cleanup(func() { r.Close(); w.Close() })
			f := newSGRMouseFilter(r)

			if _, err := io.WriteString(w, seq[:k]); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, 256)
			// First read: either nothing (prefix held) or the bare ESC at k==1.
			first, err := f.Read(buf)
			if err != nil {
				t.Fatalf("first read: %v", err)
			}
			if _, err := io.WriteString(w, seq[k:]); err != nil {
				t.Fatalf("write: %v", err)
			}
			w.Close()

			var got []byte
			got = append(got, buf[:first]...)
			for {
				n, err := f.Read(buf)
				got = append(got, buf[:n]...)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("read: %v", err)
				}
			}

			if k == 1 {
				// Split after the bare ESC: the ESC is emitted (bubbletea
				// delivers it as a harmless Escape keypress) and re-attached to
				// the body that follows in the next read.
				if string(got) != "\x1b"+canonical {
					t.Fatalf("filtered = %q, want %q", got, "\x1b"+canonical)
				}
				return
			}
			if string(got) != canonical {
				t.Fatalf("filtered = %q, want %q", got, canonical)
			}
		})
	}
}

// A lone Escape keypress (short read of a single ESC) must be delivered as-is,
// and ordinary text typed afterwards must NOT trigger a re-attach.
func TestSGRFilterLoneEscapePreserved(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	f := newSGRMouseFilter(r)

	if _, err := io.WriteString(w, "\x1b"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := string(buf[:n]); got != "\x1b" {
		t.Fatalf("first read = %q, want %q (lone ESC)", got, "\x1b")
	}

	// Ordinary text after the Escape must pass through untouched.
	if _, err := io.WriteString(w, "hello [world]"); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()
	var got []byte
	for {
		n, err := f.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if string(got) != "hello [world]" {
		t.Fatalf("second reads = %q, want %q", got, "hello [world]")
	}
}

// End-to-end burst: many wheel events with every space variant, fed through a
// real bubbletea program, must ALL arrive as MouseMsg with correct buttons and
// coordinates — and none may leak into the composer as KeyRunes/KeySpace.
// This is the regression that produced visible garbage on fast scrolling.
func TestSGRFilterThroughProgramBurst(t *testing.T) {
	probe := &mouseProbe{}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	var burst strings.Builder
	type ev struct {
		btn, x, y int
	}
	var events []ev
	for i := 0; i < 30; i++ {
		btn := 64 + i%2
		x := 30 + i
		y := 5 + i%10
		events = append(events, ev{btn, x, y})
		switch i % 4 {
		case 0:
			fmt.Fprintf(&burst, "\x1b[<%d; %d; %dM", btn, x, y)
		case 1:
			fmt.Fprintf(&burst, "\x1b[< %d ; %d ; %d M", btn, x, y)
		case 2:
			fmt.Fprintf(&burst, "\x1b[ <%d; %d; %dM", btn, x, y)
		case 3:
			fmt.Fprintf(&burst, "\x1b[<%d;%d; %dM", btn, x, y)
		}
	}
	if _, err := io.WriteString(w, burst.String()); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()

	p := tea.NewProgram(probe, tea.WithInput(newSGRMouseFilter(r)), tea.WithoutRenderer())
	go func() {
		time.Sleep(1500 * time.Millisecond)
		p.Quit()
	}()
	if _, err := p.Run(); err != nil {
		t.Fatalf("program run: %v", err)
	}

	var mice []tea.MouseWheelMsg
	for _, m := range probe.got {
		switch msg := m.(type) {
		case tea.MouseWheelMsg:
			mice = append(mice, msg)
		case tea.KeyPressMsg:
			t.Fatalf("keypress leaked into the program: %s %q", msg, msg)
		default:
			// WindowSizeMsg and other lifecycle messages are expected.
		}
	}
	if len(mice) != len(events) {
		t.Fatalf("got %d MouseWheelMsg, want %d (all: %v)", len(mice), len(events), probe.got)
	}
	for i, m := range mice {
		want := events[i]
		wantBtn := tea.MouseWheelUp
		if want.btn == 65 {
			wantBtn = tea.MouseWheelDown
		}
		if m.Button != wantBtn || m.X != want.x-1 || m.Y != want.y-1 {
			t.Fatalf("mouse[%d] = (btn %d, %d,%d), want (btn %d, %d,%d)",
				i, m.Button, m.X, m.Y, wantBtn, want.x-1, want.y-1)
		}
	}
}
