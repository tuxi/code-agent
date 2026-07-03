package websearch

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarshalBudgeted_SmallOutputUntouched(t *testing.T) {
	results := []Result{{Title: "t", URL: "https://a", Snippet: "s", Source: "tavily"}}
	b, err := marshalBudgeted("q", results)
	if err != nil {
		t.Fatal(err)
	}
	var out searchOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out.ResultsOmitted != 0 || len(out.Results) != 1 {
		t.Errorf("got omitted=%d results=%d", out.ResultsOmitted, len(out.Results))
	}
}

func TestMarshalBudgeted_DropsWholeResultsAndStaysValidJSON(t *testing.T) {
	// 30 results with maxed snippets: far beyond maxOutputBytes.
	results := make([]Result, 30)
	for i := range results {
		results[i] = Result{
			Title:   "热点新闻标题",
			URL:     "https://example.com/article",
			Snippet: strings.Repeat("热点内容", 400), // 4800 bytes, clipped to maxSnippetBytes
			Source:  "tavily",
		}
	}
	b, err := marshalBudgeted("AI 热点", results)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxOutputBytes {
		t.Errorf("output %d bytes exceeds budget %d", len(b), maxOutputBytes)
	}
	var out searchOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if out.ResultsOmitted == 0 {
		t.Error("results_omitted not declared")
	}
	if out.ResultsOmitted+len(out.Results) != 30 {
		t.Errorf("omitted %d + kept %d != 30", out.ResultsOmitted, len(out.Results))
	}
	// Every surviving result must be whole: URL intact, snippet valid UTF-8.
	for _, r := range out.Results {
		if r.URL != "https://example.com/article" {
			t.Errorf("mangled URL: %q", r.URL)
		}
		if !utf8.ValidString(r.Snippet) {
			t.Error("snippet contains invalid UTF-8")
		}
	}
}

func TestClipRunes_NeverSplitsRune(t *testing.T) {
	s := strings.Repeat("中文内容", 10) // 120 bytes
	for max := 1; max < len(s); max++ {
		if got := clipRunes(s, max); !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8", max)
		}
	}
	if got := clipRunes("short", 100); got != "short" {
		t.Errorf("fitting string changed: %q", got)
	}
}
