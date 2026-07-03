package truncate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHead_FitsUnchanged(t *testing.T) {
	if got := Head("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := Head("anything", 0); got != "anything" {
		t.Errorf("max<=0 must be a no-op, got %q", got)
	}
}

func TestHead_NeverSplitsRune(t *testing.T) {
	// 30 Chinese characters = 90 bytes; every cut point 1..89 must stay valid UTF-8.
	s := strings.Repeat("热点新闻搜索结果", 30)
	for max := 1; max < len(s); max++ {
		got := Head(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8: %q", max, got[:20])
		}
	}
}

func TestHead_CutsAtLineBoundary(t *testing.T) {
	s := strings.Repeat("https://example.com/article-one\n", 10)
	got := Head(s, 100) // 100 falls mid-line (each line is 32 bytes)
	body := got[:strings.Index(got, "\n...<truncated")]
	for _, line := range strings.Split(body, "\n") {
		if line != "" && line != "https://example.com/article-one" {
			t.Errorf("split line survived: %q", line)
		}
	}
}

func TestHead_ReportsOmittedBytes(t *testing.T) {
	s := strings.Repeat("x", 500) // single long line: mid-line cut is allowed
	got := Head(s, 100)
	if want := "...<truncated: 400 bytes omitted>"; !strings.Contains(got, want) {
		t.Errorf("marker missing %q: %q", want, got)
	}
}

func TestTail_FitsUnchanged(t *testing.T) {
	if got := Tail("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
}

func TestTail_NeverSplitsRune(t *testing.T) {
	s := strings.Repeat("构建输出日志", 30)
	for max := 1; max < len(s); max++ {
		got := Tail(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8", max)
		}
	}
}

func TestTail_StartsAtLineBoundary(t *testing.T) {
	s := strings.Repeat("line of build output here\n", 10)
	got := Tail(s, 100)
	body := got[strings.Index(got, ">\n")+2:]
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line != "line of build output here" {
			t.Errorf("split line survived: %q", line)
		}
	}
}
