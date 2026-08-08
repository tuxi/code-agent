package tui

import (
	"os"
)

// sgrMouseFilter wraps the TUI's input so SGR mouse sequences survive
// bubbletea's parser intact. Some terminals (macOS Terminal.app among others)
// pad SGR sequences with spaces — ESC [ < Cb ; Cx ; Cy M|m — with the spaces
// appearing at arbitrary positions (between the fields, around the semicolons,
// even right after the '[' or '<'). bubbletea's mouse regex accepts only the
// canonical no-space form, so a padded sequence would otherwise be mis-parsed
// as KeyRunes and typed into the composer as visible garbage, and the wheel
// event would never reach the transcript viewport.
//
// The wrapper embeds *os.File, so it still satisfies tea's term.File interface
// (io.ReadWriteCloser + Fd): bubbletea treats it as the real terminal input,
// applies raw mode to the actual stdin fd, and reads every byte through Read
// below — where padding spaces inside SGR sequences are stripped before the
// parser sees them.
//
// The filter buffers input across Read calls and only releases bytes it can
// classify: complete SGR sequences are emitted in canonical form, a trailing
// sequence prefix is held back until it completes, and everything else passes
// through byte-for-byte. No byte is ever dropped, so a burst of wheel events
// (fast scrolling) survives even when the read boundaries cut through
// sequences.
type sgrMouseFilter struct {
	*os.File
	buf []byte // bytes read from the underlying file but not yet classified
	eof bool   // underlying stream has ended (err holds the terminal error)
	err error  // io.EOF or the underlying read error

	// lastReadEndedESC is set when the previous Read emitted a chunk whose
	// last byte was a bare ESC on a short read (bubbletea treats a short read
	// as an event boundary and delivers the ESC as an Escape keypress). If the
	// next read continues with an SGR body ("[<..."), that ESC was actually the
	// start of a sequence cut by the read boundary, so it is re-attached and
	// the sequence parses as a MouseMsg instead of leaking as KeyRunes.
	lastReadEndedESC bool
}

func newSGRMouseFilter(f *os.File) *sgrMouseFilter {
	return &sgrMouseFilter{File: f}
}

func (f *sgrMouseFilter) Read(p []byte) (int, error) {
	full := false
	if !f.eof {
		tmp := make([]byte, len(p))
		n, err := f.File.Read(tmp)
		f.buf = append(f.buf, tmp[:n]...)
		full = n == len(p)
		if err != nil {
			f.eof = true
			f.err = err
		}
	}

	if len(f.buf) == 0 {
		if f.eof {
			return 0, f.err
		}
		return 0, nil
	}

	// Re-attach a split escape: if the previous short read ended with a bare
	// ESC and this read continues with an SGR body ("[<..."), the ESC was the
	// start of a sequence cut by the read boundary. Prepend it so the sequence
	// parses as a MouseMsg instead of leaking its body into the composer as
	// KeyRunes. (A bare ESC delivered on its own remains an Escape keypress;
	// the app's Escape handling at the chat page is informational and a no-op.)
	if f.lastReadEndedESC && isSGRBodyStart(f.buf) {
		f.buf = append([]byte{0x1b}, f.buf...)
	}
	f.lastReadEndedESC = false

	// A full read means more data may follow, so a trailing escape prefix can
	// be held back as the start of a sequence; a short read is an event
	// boundary, so a bare ESC there is a real keypress and must not stall.
	out, hold := normalizeSGR(f.buf, !f.eof && full)

	m := copy(p, out)
	f.buf = append(append([]byte{}, out[m:]...), hold...)

	// Remember a bare ESC emitted at the very end of this read's output: on a
	// short read it may be the start of a sequence cut by this boundary, and
	// the next read (if it continues with "[<...") needs the ESC re-attached.
	if m > 0 && m == len(out) && out[m-1] == 0x1b && (f.eof || !full) {
		f.lastReadEndedESC = true
	}

	if m == 0 && len(f.buf) > 0 {
		if !f.eof {
			// Everything buffered is an incomplete sequence prefix; wait for
			// more input rather than emitting bytes bubbletea cannot parse.
			return 0, nil
		}
		// End of stream with an incomplete sequence: release it raw.
		m = copy(p, f.buf)
		f.buf = f.buf[m:]
		if m > 0 {
			return m, nil
		}
		return 0, f.err
	}
	return m, nil
}

// normalizeSGR classifies b. out is the normalized prefix: every complete SGR
// mouse sequence has its padding spaces stripped, regardless of where they
// appear. hold is a trailing run that may be an incomplete SGR sequence — it
// must be re-examined once more input arrives. moreComing reports whether the
// caller expects further bytes (a full read that has not hit EOF); it decides
// whether a bare trailing ESC is held.
func normalizeSGR(b []byte, moreComing bool) (out, hold []byte) {
	if h := trailingSGRPrefixLen(b, moreComing); h > 0 {
		out = stripSGRSpaces(b[:len(b)-h])
		return out, append([]byte(nil), b[len(b)-h:]...)
	}
	return stripSGRSpaces(b), nil
}

// stripSGRSpaces removes padding spaces from SGR mouse sequences wherever they
// appear: between the fields, around the semicolons, between the '[' and '<',
// or right after the '<'. Everything outside a recognized sequence passes
// through byte-for-byte.
func stripSGRSpaces(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '[' {
			// Look ahead for '<', tolerating spaces between '[' and '<'.
			k := i + 2
			for k < len(b) && b[k] == ' ' {
				k++
			}
			if k < len(b) && b[k] == '<' {
				out = append(out, '\x1b', '[', '<')
				i = k + 1
				for i < len(b) {
					c := b[i]
					if c == 'M' || c == 'm' {
						out = append(out, c)
						i++
						break
					}
					if c == ' ' {
						i++ // strip padding space
						continue
					}
					if (c >= '0' && c <= '9') || c == ';' {
						out = append(out, c)
						i++
						continue
					}
					// Not an SGR mouse sequence after all: emit the byte and
					// resume normal scanning.
					out = append(out, c)
					i++
					break
				}
				continue
			}
		}
		out = append(out, b[i])
		i++
	}
	return out
}

// trailingSGRPrefixLen reports the length of a trailing run of b that is a
// plausible prefix of an SGR mouse sequence: ESC, optionally followed by '[',
// optional spaces, an optional '<', then only digits, spaces, and semicolons,
// ending exactly at the end of b. Key sequences like ESC [ 1 ; 5 D or ESC [ A
// are NOT held (the final letter breaks the pattern), which matches bubbletea's
// own buffering. A lone trailing ESC is held back only when more input is
// expected (moreComing), since a bare ESC is also a valid Escape keypress
// (bubbletea treats a short read as an event boundary and delivers it).
func trailingSGRPrefixLen(b []byte, moreComing bool) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0x1b {
			continue
		}
		j := i + 1
		if j == len(b) {
			if moreComing {
				return len(b) - i // bare ESC at the end of a full read
			}
			continue
		}
		if b[j] != '[' {
			continue
		}
		j++
		sawLess := false
		valid := true
		for ; j < len(b); j++ {
			c := b[j]
			switch {
			case c == ' ':
				// Padding space: allowed at any point in the prefix.
			case c == '<' && !sawLess:
				sawLess = true
			case c >= '0' && c <= '9':
			case c == ';':
			default:
				valid = false
			}
			if !valid {
				break
			}
		}
		if valid && j == len(b) {
			return len(b) - i
		}
	}
	return 0
}

// isSGRBodyStart reports whether b begins like the body of an SGR mouse
// sequence: '[' followed, possibly through padding spaces, by '<'. Used to
// re-attach a split ESC only when the continuation really is a sequence body,
// never for ordinary text typed after an Escape keypress.
func isSGRBodyStart(b []byte) bool {
	if len(b) < 2 || b[0] != '[' {
		return false
	}
	for i := 1; i < len(b); i++ {
		if b[i] == '<' {
			return true
		}
		if b[i] != ' ' {
			return false
		}
	}
	return false
}
