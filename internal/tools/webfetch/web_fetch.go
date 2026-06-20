// Package webfetch provides the web_fetch tool — fetch, clean, and convert a
// web page into LLM-readable markdown and plain text.
package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/tools"
	"code-agent/internal/web/cache"

	"golang.org/x/net/html"
)

// Tool implements the web_fetch tool.
type Tool struct {
	client *http.Client
	cache  *cache.Cache
}

// NewTool builds a web_fetch tool from the web section of config.
func NewTool(cfg app.WebConfig) *Tool {
	timeout := cfg.Fetch.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	ttl := time.Duration(cfg.Fetch.CacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	return &Tool{
		client: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
		cache: cache.New(ttl),
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

func (t *Tool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
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

	// Truncate large JSON payloads to avoid context blowout.
	const maxJSON = 50000
	if len(pretty) > maxJSON {
		pretty = pretty[:maxJSON]
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
