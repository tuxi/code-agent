package webfetch

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestExtractSummary_MetaDescription(t *testing.T) {
	raw := `<html><head><meta name="description" content="This is a comprehensive guide to Redis GEO sorted set implementation covering geohash internals."></head><body><p>Short.</p></body></html>`
	rawDoc, _ := html.Parse(strings.NewReader(raw))
	cleaned := cleanHTML(rawDoc)

	summary, _, _ := compress(rawDoc, cleaned)
	if summary == "" {
		t.Fatal("expected summary from meta description")
	}
	if !strings.Contains(summary, "Redis") {
		t.Errorf("summary should contain content: %s", summary)
	}
}

func TestExtractSummary_FallbackToParagraph(t *testing.T) {
	raw := `<html><head><title>Test</title></head><body><article><p>This is a long enough paragraph that contains substantial technical content about Kubernetes pod networking and CNI plugins configuration.</p></article></body></html>`
	rawDoc, _ := html.Parse(strings.NewReader(raw))
	cleaned := cleanHTML(rawDoc)

	summary, _, _ := compress(rawDoc, cleaned)
	if !strings.Contains(summary, "Kubernetes") {
		t.Errorf("expected paragraph fallback: %s", summary)
	}
}

func TestExtractKeyPoints_FromHeadings(t *testing.T) {
	html_str := `<article>
		<h2>Installation</h2>
		<h3>Prerequisites</h3>
		<h2>Configuration</h2>
		<h4>Advanced Options</h4>
		<p>Some content.</p>
	</article>`
	doc, _ := html.Parse(strings.NewReader(html_str))
	cleaned := cleanHTML(doc)

	_, keyPoints, _ := compress(doc, cleaned)
	if len(keyPoints) < 3 {
		t.Fatalf("expected at least 3 key points from headings, got %d: %v", len(keyPoints), keyPoints)
	}
	if !contains(keyPoints, "Installation") || !contains(keyPoints, "Configuration") {
		t.Errorf("missing expected headings: %v", keyPoints)
	}
}

func TestExtractKeyPoints_ListFallback(t *testing.T) {
	html_str := `<article>
		<h2>Main</h2>
		<ul>
			<li>First important point about the system architecture.</li>
			<li>Second key consideration for deployment.</li>
			<li>Third critical security concern.</li>
		</ul>
	</article>`
	doc, _ := html.Parse(strings.NewReader(html_str))
	cleaned := cleanHTML(doc)

	_, keyPoints, _ := compress(doc, cleaned)
	// Should have Main + 3 list items = 4 key points
	if len(keyPoints) < 2 {
		t.Fatalf("expected key points from list fallback, got %d: %v", len(keyPoints), keyPoints)
	}
}

func TestExtractCodeBlocks(t *testing.T) {
	html_str := `<article>
		<pre><code class="language-go">func main() {
	fmt.Println("hello")
}</code></pre>
		<pre><code>plain code</code></pre>
	</article>`
	doc, _ := html.Parse(strings.NewReader(html_str))
	cleaned := cleanHTML(doc)

	_, _, blocks := compress(doc, cleaned)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 code blocks, got %d", len(blocks))
	}
	if blocks[0].Language != "go" {
		t.Errorf("expected 'go' language, got %q", blocks[0].Language)
	}
	if !strings.Contains(blocks[0].Code, "func main()") {
		t.Errorf("expected code content: %s", blocks[0].Code)
	}
	if blocks[1].Language != "" {
		t.Errorf("expected empty language for plain code block, got %q", blocks[1].Language)
	}
}

func TestExtractCodeBlocks_Capped(t *testing.T) {
	// Generate 15 code blocks, should cap at 10.
	var sb strings.Builder
	sb.WriteString("<article>")
	for i := 0; i < 15; i++ {
		sb.WriteString("<pre><code>block ")
		sb.WriteString(string(rune('0' + i%10)))
		sb.WriteString("</code></pre>")
	}
	sb.WriteString("</article>")
	doc, _ := html.Parse(strings.NewReader(sb.String()))
	cleaned := cleanHTML(doc)

	_, _, blocks := compress(doc, cleaned)
	if len(blocks) > maxCodeBlocks {
		t.Errorf("expected at most %d code blocks, got %d", maxCodeBlocks, len(blocks))
	}
}

func TestTruncateRunes(t *testing.T) {
	s := "hello world this is a test"
	result := truncateRunes(s, 10)
	if len([]rune(result)) > 10+1 { // +1 for ellipsis
		t.Errorf("expected truncated to ~10 runes + '…', got %q (len=%d)", result, len([]rune(result)))
	}
	if !strings.HasSuffix(result, "…") {
		t.Errorf("expected ellipsis suffix: %q", result)
	}
}

func contains(slice []string, s string) bool {
	for _, item := range slice {
		if strings.Contains(item, s) {
			return true
		}
	}
	return false
}
