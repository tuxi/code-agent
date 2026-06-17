package main

import "io"

// Bracketed-paste markers. When enabled (ESC [ ? 2004 h), a terminal wraps
// pasted text in these so a program can tell a paste from typing.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// pasteFilter wraps stdin to neutralize multi-line pastes: it strips the
// bracketed-paste markers and, while inside a paste, turns embedded newlines
// into spaces. Without this, chzyer/readline (which has no paste support)
// submits each pasted line as a separate input — spawning a runaway turn per
// line and bleeding leftover lines into y/N approval prompts.
type pasteFilter struct {
	src     io.Reader
	inPaste bool
	out     []byte // filtered bytes ready to hand to the reader
	carry   []byte // bytes held back as a possible partial marker
}

func newPasteFilter(src io.Reader) *pasteFilter { return &pasteFilter{src: src} }

func (f *pasteFilter) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		buf := make([]byte, 1024)
		n, err := f.src.Read(buf)
		if n > 0 {
			f.carry = append(f.carry, buf[:n]...)
			f.scan(false)
		}
		if err != nil {
			f.scan(true) // flush: a trailing partial marker becomes literal
			if len(f.out) == 0 {
				return 0, err
			}
			break
		}
		// If everything read so far is a held partial marker, loop and read more.
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// Close is a no-op: the real stdin must never be closed out from under the
// process.
func (f *pasteFilter) Close() error { return nil }

// StdinWrapper returns a ReadCloser wrapping the filter so it can be plugged
// into readline's Config.Stdin. The Close is a no-op (see above); the caller
// must separately call Close on the real source when it is done.
func (f *pasteFilter) StdinWrapper() io.ReadCloser { return f }

// scan drains carry into out. With flush=true a trailing partial marker is
// emitted as ordinary bytes (used at EOF).
func (f *pasteFilter) scan(flush bool) {
	for len(f.carry) > 0 {
		switch {
		case hasBytePrefix(f.carry, pasteStart):
			f.inPaste = true
			f.carry = f.carry[len(pasteStart):]
		case hasBytePrefix(f.carry, pasteEnd):
			f.inPaste = false
			f.carry = f.carry[len(pasteEnd):]
		case !flush && (isMarkerPrefix(f.carry, pasteStart) || isMarkerPrefix(f.carry, pasteEnd)):
			return // could still grow into a marker; wait for more bytes
		default:
			b := f.carry[0]
			f.carry = f.carry[1:]
			if f.inPaste && (b == '\n' || b == '\r') {
				b = ' '
			}
			f.out = append(f.out, b)
		}
	}
}

func hasBytePrefix(b []byte, s string) bool {
	if len(b) < len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

// isMarkerPrefix reports whether b is a strict, non-empty prefix of marker — so
// b might still grow into the full marker once more bytes arrive.
func isMarkerPrefix(b []byte, marker string) bool {
	if len(b) == 0 || len(b) >= len(marker) {
		return false
	}
	for i := 0; i < len(b); i++ {
		if b[i] != marker[i] {
			return false
		}
	}
	return true
}
