// Package webfetch provides the web_fetch tool — fetch, clean, and convert a
// web page into LLM-readable markdown and plain text.
package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/tools"
	"code-agent/internal/truncate"
	"code-agent/internal/web/cache"

	"golang.org/x/net/html"
)

// errBlockedAddress marks an SSRF-blocked dial. web_fetch never falls back to a
// server-side extractor on this — doing so would hand an internal URL to a third
// party.
var errBlockedAddress = errors.New("blocked non-public address")

// contentFallback retrieves a URL's content through a server-side service when a
// direct fetch can't reach the host (e.g. the URL is blocked from this network).
type contentFallback interface {
	Extract(ctx context.Context, url string) (string, error)
}

// Tool implements the web_fetch tool.
type Tool struct {
	client   *http.Client
	cache    *cache.Cache
	fallback contentFallback // optional; nil means direct-fetch only
}

// NewTool builds a web_fetch tool from the web section of config. The HTTP
// client it builds always blocks non-public dial targets (SSRF protection).
func NewTool(cfg app.WebConfig) *Tool {
	return newTool(cfg, false)
}

// newTool is the internal constructor. allowPrivate disables the SSRF dial
// guard and is for tests only (httptest servers bind to loopback); production
// always reaches this via NewTool with allowPrivate=false.
func newTool(cfg app.WebConfig, allowPrivate bool) *Tool {
	timeout := cfg.Fetch.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	ttl := time.Duration(cfg.Fetch.CacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	// Clone the default transport so we keep its proxy/TLS defaults, then inject
	// a dial-time SSRF guard that runs after DNS resolution for every connection.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   dialGuard(allowPrivate),
	}).DialContext

	t := &Tool{
		client: &http.Client{
			Timeout:   time.Duration(timeout) * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("web_fetch: stopped after 5 redirects")
				}
				if s := req.URL.Scheme; s != "http" && s != "https" {
					return fmt.Errorf("web_fetch: disallowed redirect scheme %q", s)
				}
				return nil
			},
		},
		cache: cache.New(ttl),
	}
	// Server-side fetch fallback: when a direct fetch can't reach a host (blocked
	// network / GFW), retrieve content via Tavily /extract using the same key as
	// web_search. Optional — no key means direct-only.
	if key := cfg.Search.TavilyAPIKey(); key != "" {
		t.fallback = newTavilyExtractor(key, timeout)
	}
	return t
}

// dialGuard returns a net.Dialer Control func that runs at TCP dial time —
// after DNS resolution, for every connection including redirect hops — and
// rejects any non-public destination. Validating the *resolved IP* here
// (rather than the URL host) is what defeats both redirect-to-internal and
// DNS-rebinding SSRF: every dial passes through this hook with the concrete IP
// about to be contacted.
func dialGuard(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("web_fetch: cannot parse dial address %q", host)
		}
		// IsLinkLocalUnicast covers 169.254.0.0/16 (cloud metadata) and
		// fe80::/10; IsPrivate covers RFC 1918 and IPv6 unique-local (fc00::/7).
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("web_fetch: %w %s", errBlockedAddress, ip)
		}
		return nil
	}
}

func (t *Tool) Name() string { return "web_fetch" }

func (t *Tool) Description() string {
	return "Fetch and clean a web page, returning LLM-readable markdown and plain text. " +
		"Preserves code blocks, tables, and lists while removing ads, navigation, and " +
		"other boilerplate. Use this after web_search to read the full content of a " +
		"promising result."
}

func (t *Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"url": {Type: "string", Description: "The URL to fetch and clean."},
	}, "url").JSON()
}

// fetchOutput is the structured result from web_fetch, returned as JSON.
// Summary and key_points are the primary reasoning input for the LLM; markdown
// is the full fallback. Code blocks are extracted separately for structured
// access (they are also present in markdown).
type fetchOutput struct {
	Title      string      `json:"title"`
	Summary    string      `json:"summary"`
	KeyPoints  []string    `json:"key_points"`
	CodeBlocks []codeBlock `json:"code_blocks"`
	Content    string      `json:"content"`
	Markdown   string      `json:"markdown"`
	Links      []string    `json:"links"`
}

func (t *Tool) Execute(ctx context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in struct {
		URL string `json:"url"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid web_fetch input: %w", err)
		}
	}

	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		return tools.ToolResult{}, fmt.Errorf("url is required")
	}

	out, err := t.fetch(ctx, in.URL)
	if err != nil {
		return tools.ToolResult{}, err
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("marshal output: %w", err)
	}

	return tools.ToolResult{Content: string(b)}, nil
}

func (t *Tool) fetch(ctx context.Context, urlStr string) (*fetchOutput, error) {
	// 0. Only http/https. The dial-time guard (blockPrivateIP) covers internal
	// addresses and redirects; this rejects file://, gopher://, etc. up front.
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("fetch: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("fetch: unsupported URL scheme %q (only http/https allowed)", parsed.Scheme)
	}

	// 1. Check cache.
	if entry, ok := t.cache.Load(urlStr); ok {
		return t.parseHTML(entry.Body, urlStr)
	}

	// 2. Fetch with timeout.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: invalid URL: %w", err)
	}
	req.Header.Set("User-Agent", "CodeAgent/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")

	resp, err := t.client.Do(req)
	if err != nil {
		// Direct fetch failed at the transport layer (unreachable / blocked /
		// timeout). Try the server-side fallback — but never on an SSRF block,
		// which would leak the internal URL to a third party.
		if t.fallback != nil && !errors.Is(err, errBlockedAddress) {
			if out, ferr := t.fetchViaFallback(ctx, urlStr); ferr == nil {
				return out, nil
			}
		}
		return nil, fmt.Errorf("fetch: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	isHTML := strings.HasPrefix(strings.ToLower(ct), "text/html") ||
		strings.HasPrefix(strings.ToLower(ct), "application/xhtml")
	isJSON := strings.HasPrefix(strings.ToLower(ct), "application/json")
	if ct != "" && !isHTML && !isJSON {
		return nil, fmt.Errorf("fetch: unsupported content type: %s", ct)
	}

	// Read body with a cap to avoid OOM on huge pages.
	const maxBody = 5 * 1024 * 1024 // 5 MiB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("fetch: read body: %w", err)
	}

	// Cache before parsing.
	t.cache.Store(urlStr, body, ct)

	// JSON path: pretty-print as a markdown code block.
	if isJSON {
		return t.parseJSON(body, urlStr)
	}
	return t.parseHTML(body, urlStr)
}

// fetchViaFallback retrieves urlStr through the configured server-side extractor
// (Tavily /extract). The content comes back already cleaned, so it bypasses the
// HTML pipeline. Used only when a direct fetch can't reach the host.
func (t *Tool) fetchViaFallback(ctx context.Context, urlStr string) (*fetchOutput, error) {
	content, err := t.fallback.Extract(ctx, urlStr)
	if err != nil {
		return nil, err
	}
	const maxFallback = 200_000 // bytes — guard against a huge page blowing context
	content = truncate.Head(content, maxFallback)
	return &fetchOutput{
		Title:      urlStr,
		Summary:    "Direct fetch failed; content retrieved via server-side extractor (Tavily).",
		KeyPoints:  []string{},
		CodeBlocks: []codeBlock{},
		Content:    content,
		Markdown:   content,
		Links:      []string{},
	}, nil
}

func (t *Tool) parseHTML(body []byte, _ string) (*fetchOutput, error) {
	rawDoc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("fetch: HTML parse error: %w", err)
	}

	cleaned := cleanHTML(rawDoc)
	title := extractTitle(rawDoc)
	summary, keyPoints, codeBlocks := compress(rawDoc, cleaned)
	markdown := htmlToMarkdown(cleaned)
	plainText := htmlToPlainText(cleaned)
	links := extractLinks(rawDoc)
	if links == nil {
		links = []string{}
	}
	if keyPoints == nil {
		keyPoints = []string{}
	}
	if codeBlocks == nil {
		codeBlocks = []codeBlock{}
	}

	return &fetchOutput{
		Title:      title,
		Summary:    summary,
		KeyPoints:  keyPoints,
		CodeBlocks: codeBlocks,
		Content:    plainText,
		Markdown:   markdown,
		Links:      links,
	}, nil
}

// parseJSON handles application/json responses. The JSON is pretty-printed and
// wrapped in a markdown code block so the LLM can reason about it.
func (t *Tool) parseJSON(body []byte, urlStr string) (*fetchOutput, error) {
	// Try to pretty-print.
	pretty := body
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if formatted, err := json.MarshalIndent(parsed, "", "  "); err == nil {
			pretty = formatted
		}
	}

	// Truncate large JSON payloads to avoid context blowout. Line-safe head cut
	// with an explicit marker: the model sees that (and how much) is missing
	// instead of a silently amputated document.
	const maxJSON = 50000
	if len(pretty) > maxJSON {
		pretty = []byte(truncate.Head(string(pretty), maxJSON))
	}

	markdown := "```json\n" + string(pretty) + "\n```\n"
	summary := "JSON API response from " + urlStr
	if len(pretty) < 500 {
		summary = string(pretty)
	}

	return &fetchOutput{
		Title:      urlStr,
		Summary:    summary,
		KeyPoints:  []string{},
		CodeBlocks: []codeBlock{{Language: "json", Code: string(pretty)}},
		Content:    string(pretty),
		Markdown:   markdown,
		Links:      []string{},
	}, nil
}
