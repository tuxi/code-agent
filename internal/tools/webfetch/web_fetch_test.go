package webfetch

import (
	"code-agent/internal/app"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestWebFetchBasic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html><head><title>Test Page</title></head>
<body>
  <header>Site header</header>
  <nav>Home | About</nav>
  <article>
    <h1>Hello World</h1>
    <p>This is a <strong>test</strong> paragraph.</p>
    <pre><code>func main() {
    fmt.Println("hello")
}</code></pre>
    <ul>
      <li>Item 1</li>
      <li>Item 2</li>
    </ul>
    <p>Another paragraph with a <a href="https://example.com">link</a>.</p>
  </article>
  <footer>Site footer</footer>
</body></html>`))
	}))
	defer srv.Close()

	tool := newTool(app.WebConfig{}, true)
	result, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out fetchOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, result.Content)
	}

	if out.Title != "Test Page" {
		t.Errorf("expected title 'Test Page', got %q", out.Title)
	}
	// Markdown should contain the article content, not header/footer.
	if strings.Contains(out.Markdown, "Site header") {
		t.Error("markdown should not contain header text")
	}
	if strings.Contains(out.Markdown, "Site footer") {
		t.Error("markdown should not contain footer text")
	}
	// Code block should be preserved.
	if !strings.Contains(out.Markdown, "```") {
		t.Error("markdown should contain code block fences")
	}
	if !strings.Contains(out.Markdown, "func main()") {
		t.Error("markdown should contain code content")
	}
	// Bold should be preserved.
	if !strings.Contains(out.Markdown, "**test**") {
		t.Errorf("markdown should contain bold: %s", out.Markdown)
	}
	// Link should be preserved.
	if !strings.Contains(out.Markdown, "[link]") {
		t.Errorf("markdown should contain link: %s", out.Markdown)
	}
	// Plain text should not have markdown syntax.
	if strings.Contains(out.Content, "**") {
		t.Error("plain text should not contain bold markers")
	}
	// Links should be extracted.
	if len(out.Links) == 0 {
		t.Error("links should be extracted")
	}
}

func TestWebFetchMissingURL(t *testing.T) {
	tool := NewTool(app.WebConfig{})
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Errorf("expected url error, got: %v", err)
	}
}

func TestWebFetchHTMLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tool := newTool(app.WebConfig{}, true)
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 error, got: %v", err)
	}
}

func TestMarkdownConversion(t *testing.T) {
	html := `<h1>Title</h1><p>A paragraph with <strong>bold</strong> and <em>italic</em>.</p>`
	doc, err := parseHTMLString(html)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	md := htmlToMarkdown(doc)
	if !strings.Contains(md, "# Title") {
		t.Errorf("expected heading: %s", md)
	}
	if !strings.Contains(md, "**bold**") {
		t.Errorf("expected bold: %s", md)
	}
	if !strings.Contains(md, "*italic*") {
		t.Errorf("expected italic: %s", md)
	}
}

func TestCodeBlockPreservation(t *testing.T) {
	html := `<pre><code class="language-go">func main() {}</code></pre>`
	doc, err := parseHTMLString(html)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	md := htmlToMarkdown(doc)
	if !strings.Contains(md, "```go") {
		t.Errorf("expected code fence with language: %s", md)
	}
	if !strings.Contains(md, "func main() {}") {
		t.Errorf("expected code content: %s", md)
	}
}

func TestCache(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><p>cached</p></body></html>`))
	}))
	defer srv.Close()

	tool := newTool(app.WebConfig{}, true)

	// First call — should fetch.
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 fetch, got %d", callCount)
	}

	// Second call — should hit cache.
	_, err = tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected cache hit (1 fetch), got %d", callCount)
	}
}

func TestTableToMarkdown(t *testing.T) {
	html := `<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>`
	doc, err := parseHTMLString(html)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	md := htmlToMarkdown(doc)
	if !strings.Contains(md, "A") && !strings.Contains(md, "B") && !strings.Contains(md, "1") && !strings.Contains(md, "2") {
		t.Errorf("table content should be preserved: %s", md)
	}
}

func TestAdRemoval(t *testing.T) {
	html := `<div class="advertisement">Buy this!</div><p>Real content.</p>`
	doc, err := parseHTMLString(html)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	cleaned := cleanHTML(doc)
	md := htmlToMarkdown(cleaned)
	if strings.Contains(md, "Buy this") {
		t.Error("ad content should be removed")
	}
	if !strings.Contains(md, "Real content") {
		t.Error("real content should be preserved")
	}
}

func TestCookieBannerRemoval(t *testing.T) {
	html := `<div class="cookie-banner">Accept cookies</div><p>Content.</p>`
	doc, err := parseHTMLString(html)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	cleaned := cleanHTML(doc)
	md := htmlToMarkdown(cleaned)
	if strings.Contains(md, "Accept cookies") {
		t.Error("cookie banner should be removed")
	}
	if !strings.Contains(md, "Content") {
		t.Error("content should be preserved")
	}
}

// TestSSRF_BlocksLoopback verifies the production constructor (NewTool) refuses
// to dial a loopback address — the core SSRF protection. It uses a real
// httptest server (which binds to 127.0.0.1) so the guard is exercised at the
// dial layer, exactly as it would be for a malicious redirect to the metadata IP.
func TestSSRF_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never reach here"))
	}))
	defer srv.Close()

	tool := NewTool(app.WebConfig{}) // production: SSRF guard active
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err == nil || !strings.Contains(err.Error(), "blocked non-public address") {
		t.Errorf("expected loopback to be blocked, got: %v", err)
	}
}

// TestSSRF_RejectsNonHTTPScheme verifies non-http(s) URLs are rejected up front,
// before any dial — covering file://, gopher://, etc.
func TestSSRF_RejectsNonHTTPScheme(t *testing.T) {
	tool := NewTool(app.WebConfig{})
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"url":"file:///etc/passwd"}`))
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("expected scheme rejection, got: %v", err)
	}
}

func parseHTMLString(s string) (*html.Node, error) {
	return html.Parse(strings.NewReader(s))
}
