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
	"math/rand/v2"
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

// Retry and backoff tuning for direct fetches.
const (
	maxFetchAttempts = 3 // 1 initial + 2 retries
	fetchBaseBackoff = 500 * time.Millisecond
	fetchMaxBackoff  = 5 * time.Second
)

// fetchBackoff returns a full-jitter delay before the n-th retry (1-indexed).
func fetchBackoff(n int) time.Duration {
	d := fetchBaseBackoff
	for i := 1; i < n; i++ {
		d *= 2
		if d > fetchMaxBackoff {
			return fetchMaxBackoff
		}
	}
	// Full jitter: uniform pick in [d/2, d].
	half := d / 2
	return half + time.Duration(rand.N(int(half+1)))
}

// isRetryableNetError reports whether a transport-level error from http.Client.Do
// is worth retrying. SSRF blocks and caller cancellations are never retryable.
func isRetryableNetError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errBlockedAddress) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Catch connection-refused / connection-reset BEFORE the net.Error check
	// because *url.Error implements net.Error but reports Temporary()=false for
	// ECONNREFUSED on macOS (and some Linux kernels). A refused connection is
	// transient — the server may recover between retries.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	return false
}

// isRetryableStatus reports whether an HTTP status code is worth retrying.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// sleepWithBackoff blocks for a jittered exponential-backoff delay, or until ctx
// is cancelled.
func sleepWithBackoff(ctx context.Context, attempt int) error {
	d := fetchBaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > fetchMaxBackoff {
			d = fetchMaxBackoff
			break
		}
	}
	half := d / 2
	jittered := half + time.Duration(rand.N(int(half+1)))

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(jittered):
		return nil
	}
}

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

	// 2. Fetch with retries. Transient network errors and retryable HTTP status
	// codes (408, 429, 5xx) get up to maxFetchAttempts total attempts with
	// jittered exponential backoff. A server-side fallback (Tavily /extract) is
	// tried once on the first retryable failure — but never on an SSRF block,
	// which would leak the internal URL to a third party.
	var lastErr error
	fallbackTried := false

	for attempt := 0; attempt < maxFetchAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepWithBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}

		out, retryable, err := t.doFetch(ctx, urlStr)
		if err == nil {
			return out, nil
		}
		lastErr = err

		// On the first retryable failure, try the server-side fallback. The
		// retryable gate already excludes SSRF blocks (isRetryableNetError
		// returns false for errBlockedAddress), so a blocked address never
		// leaks to a third party.
		if !fallbackTried && t.fallback != nil && retryable {
			fallbackTried = true
			if out, ferr := t.fetchViaFallback(ctx, urlStr); ferr == nil {
				return out, nil
			}
		}

		if !retryable {
			return nil, lastErr
		}
	}

	return nil, fmt.Errorf("fetch: request failed after %d attempts: %w", maxFetchAttempts, lastErr)
}

// doFetch performs a single HTTP GET and processes the response. The bool return
// indicates whether an error is transient — worth retrying or falling back.
func (t *Tool) doFetch(ctx context.Context, urlStr string) (*fetchOutput, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, false, fmt.Errorf("fetch: invalid URL: %w", err)
	}
	// Use a common browser User-Agent — many sites return 403 or a CAPTCHA for
	// obviously non-browser UAs like "CodeAgent/1.0".
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, isRetryableNetError(err), fmt.Errorf("fetch: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, isRetryableStatus(resp.StatusCode), fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	isHTML := strings.HasPrefix(strings.ToLower(ct), "text/html") ||
		strings.HasPrefix(strings.ToLower(ct), "application/xhtml")
	isJSON := strings.HasPrefix(strings.ToLower(ct), "application/json")
	if ct != "" && !isHTML && !isJSON {
		return nil, false, fmt.Errorf("fetch: unsupported content type: %s", ct)
	}

	// Read body with a cap to avoid OOM on huge pages.
	const maxBody = 5 * 1024 * 1024 // 5 MiB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, isRetryableNetError(err), fmt.Errorf("fetch: read body: %w", err)
	}

	// Cache before parsing.
	t.cache.Store(urlStr, body, ct)

	// JSON path: pretty-print as a markdown code block.
	if isJSON {
		out, err := t.parseJSON(body, urlStr)
		return out, false, err
	}
	out, err := t.parseHTML(body, urlStr)
	return out, false, err
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
