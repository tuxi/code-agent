package main

import (
	"io"
	"strings"
	"testing"
)

func filtered(t *testing.T, src io.Reader) string {
	t.Helper()
	out, err := io.ReadAll(newPasteFilter(src))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// oneByteReader returns its data one byte per Read, to exercise markers split
// across reads (the carry path).
type oneByteReader struct {
	data []byte
	i    int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.i]
	r.i++
	return 1, nil
}

func TestPasteFilterPassesThroughNormalInput(t *testing.T) {
	// No paste markers: bytes (including a real newline) are untouched.
	if got := filtered(t, strings.NewReader("hello world\n")); got != "hello world\n" {
		t.Fatalf("got %q, want %q", got, "hello world\\n")
	}
}

func TestPasteFilterFlattensPaste(t *testing.T) {
	in := pasteStart + "line1\nline2\nline3" + pasteEnd
	if got := filtered(t, strings.NewReader(in)); got != "line1 line2 line3" {
		t.Fatalf("got %q, want %q", got, "line1 line2 line3")
	}
}

func TestPasteFilterKeepsRealEnterAfterPaste(t *testing.T) {
	// Typed text, then a multi-line paste, then a real Enter to submit. Only the
	// final newline (outside the paste) survives as a submit.
	in := "ask: " + pasteStart + "a\nb" + pasteEnd + "\n"
	if got := filtered(t, strings.NewReader(in)); got != "ask: a b\n" {
		t.Fatalf("got %q, want %q", got, "ask: a b\\n")
	}
}

func TestPasteFilterHandlesMarkersSplitAcrossReads(t *testing.T) {
	in := []byte("x" + pasteStart + "1\n2" + pasteEnd + "y")
	got := filtered(t, &oneByteReader{data: in})
	if got != "x1 2y" {
		t.Fatalf("byte-split got %q, want %q", got, "x1 2y")
	}
}

func TestPasteFilterLeavesOtherEscapesIntact(t *testing.T) {
	// An arrow-key escape (ESC [ C) is not a paste marker and must pass through
	// so readline can act on it.
	in := "a\x1b[Cb"
	if got := filtered(t, strings.NewReader(in)); got != "a\x1b[Cb" {
		t.Fatalf("got %q, want %q", got, "a\\x1b[Cb")
	}
}
