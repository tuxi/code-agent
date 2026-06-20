package webfetch

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// codeBlock is a single extracted code snippet with its language hint.
type codeBlock struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

const maxCodeBlocks = 10
const maxCodeLen = 2000
const maxKeyPoints = 8
const maxKeyPointLen = 200

// compress extracts structured context from a cleaned HTML document and the
// original document (for <meta> tags). It returns summary, key_points, and
// code_blocks. All extraction is deterministic — no model call.
func compress(rawDoc, cleaned *html.Node) (summary string, keyPoints []string, blocks []codeBlock) {
	summary = extractSummary(rawDoc, cleaned)
	keyPoints = extractKeyPoints(cleaned)
	blocks = extractCodeBlocks(cleaned)
	return
}

// extractSummary picks the best summary text, in priority order:
//  1. <meta name="description"> content (if ≥ 50 chars)
//  2. First substantial <p> in the main content (≥ 80 chars)
//  3. First <h1> + its following <p> concatenated
//  4. First 200 chars of text content
func extractSummary(rawDoc, cleaned *html.Node) string {
	// 1. Meta description.
	if rawDoc != nil {
		if desc := findMetaDescription(rawDoc); desc != "" && len([]rune(desc)) >= 50 {
			return truncateRunes(desc, 300)
		}
	}

	// 2. First substantial <p>.
	if p := findFirstSubstantialP(cleaned, 80); p != nil {
		return truncateRunes(strings.TrimSpace(textContent(p)), 300)
	}

	// 3. First <h1> + next <p>.
	if h1 := findFirst(cleaned, atom.H1); h1 != nil {
		h1Text := strings.TrimSpace(textContent(h1))
		if next := nextElement(h1); next != nil && next.DataAtom == atom.P {
			pText := strings.TrimSpace(textContent(next))
			return truncateRunes(h1Text+". "+pText, 300)
		}
		return truncateRunes(h1Text, 300)
	}

	// 4. Fallback: first 200 chars.
	allText := strings.TrimSpace(textContent(cleaned))
	return truncateRunes(allText, 200)
}

func findMetaDescription(doc *html.Node) string {
	var desc string
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Meta {
			nameLower := ""
			content := ""
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				if key == "name" && strings.ToLower(a.Val) == "description" {
					nameLower = "description"
				}
				if key == "content" {
					content = a.Val
				}
			}
			if nameLower == "description" && content != "" {
				desc = content
			}
		}
	})
	return desc
}

func findFirstSubstantialP(n *html.Node, minLen int) *html.Node {
	var found *html.Node
	walk(n, func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.P {
			t := strings.TrimSpace(textContent(n))
			if len([]rune(t)) >= minLen {
				found = n
			}
		}
	})
	return found
}

// nextElement returns the next sibling element node, skipping text nodes.
func nextElement(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode {
			return s
		}
	}
	return nil
}

// extractKeyPoints pulls structural headings and list items from the content.
func extractKeyPoints(cleaned *html.Node) []string {
	var points []string

	// 1. Collect H2-H4 headings as primary key points.
	walk(cleaned, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.DataAtom {
		case atom.H2, atom.H3, atom.H4:
			t := strings.TrimSpace(textContent(n))
			if t != "" && len(points) < maxKeyPoints {
				points = append(points, truncateRunes(t, maxKeyPointLen))
			}
		}
	})

	// 2. If fewer than 3 headings found, supplement with list items.
	if len(points) < 3 {
		walk(cleaned, func(n *html.Node) {
			if n.Type != html.ElementNode || n.DataAtom != atom.Li {
				return
			}
			if len(points) >= maxKeyPoints {
				return
			}
			t := firstSentence(n, maxKeyPointLen)
			if t != "" {
				points = append(points, t)
			}
		})
	}

	return points
}

// firstSentence returns the first sentence of a node's text content, trimmed.
func firstSentence(n *html.Node, maxLen int) string {
	full := strings.TrimSpace(textContent(n))
	if full == "" {
		return ""
	}
	// Find first sentence boundary: .!? followed by space/end or newline.
	for i, r := range full {
		if r == '.' || r == '!' || r == '?' || r == '。' || r == '！' || r == '？' {
			s := strings.TrimSpace(full[:i+1])
			if len([]rune(s)) >= 10 {
				return truncateRunes(s, maxLen)
			}
		}
	}
	return truncateRunes(full, maxLen)
}

// extractCodeBlocks collects <pre><code> blocks with language hints.
func extractCodeBlocks(cleaned *html.Node) []codeBlock {
	var blocks []codeBlock
	walk(cleaned, func(n *html.Node) {
		if n.Type != html.ElementNode || n.DataAtom != atom.Pre {
			return
		}
		if len(blocks) >= maxCodeBlocks {
			return
		}
		var codeNode *html.Node
		walk(n, func(c *html.Node) {
			if codeNode == nil && c.Type == html.ElementNode && c.DataAtom == atom.Code {
				codeNode = c
			}
		})
		if codeNode == nil {
			return
		}
		lang := detectLanguage(codeNode)
		code := textContent(codeNode)
		code = strings.TrimRight(code, "\n\r")
		code = truncateRunes(code, maxCodeLen)
		if code != "" {
			blocks = append(blocks, codeBlock{Language: lang, Code: code})
		}
	})
	return blocks
}

func detectLanguage(codeNode *html.Node) string {
	for _, a := range codeNode.Attr {
		if strings.EqualFold(a.Key, "class") {
			for _, part := range strings.Fields(a.Val) {
				part = strings.TrimPrefix(part, "language-")
				part = strings.TrimPrefix(part, "lang-")
				if part != "" && part != "code" {
					return part
				}
			}
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// isWhitespaceOnly returns true if s contains only whitespace.
func isWhitespaceOnly(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
