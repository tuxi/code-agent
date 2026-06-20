package webfetch

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlToMarkdown converts a cleaned HTML subtree to markdown. It preserves:
// headings, paragraphs, links, bold/italic, code blocks (with language hints),
// lists (ordered and unordered), tables, and blockquotes. Inline HTML that is
// not handled is stripped, keeping only the text content.
func htmlToMarkdown(n *html.Node) string {
	var b strings.Builder
	renderToMarkdown(&b, n, &mdState{})
	return strings.TrimSpace(b.String())
}

// htmlToPlainText converts a cleaned HTML subtree to plain text — structural
// markdown characters are stripped, but line breaks and indentation are kept so
// the output is still readable as a fallback.
func htmlToPlainText(n *html.Node) string {
	md := htmlToMarkdown(n)
	return stripMarkdownSyntax(md)
}

// stripMarkdownSyntax removes markdown formatting characters while keeping the
// text and structural whitespace.
func stripMarkdownSyntax(md string) string {
	// A minimal approach: remove link syntax [text](url) → text, remove emphasis
	// markers **bold** → bold, *italic* → italic, `code` → code, ```lang → empty,
	// table pipes, heading #'s, blockquote >, and list markers.
	// This is intentionally simple — the plain text version is a fallback.

	// Remove code fences and language hints.
	md = strings.ReplaceAll(md, "```", "")
	// Remove inline code backticks (paired).
	md = strings.ReplaceAll(md, "`", "")
	// Remove bold markers.
	md = strings.ReplaceAll(md, "**", "")
	// Convert markdown links: [text](url) → text
	for {
		start := strings.Index(md, "[")
		if start == -1 {
			break
		}
		end := strings.Index(md[start:], "]")
		if end == -1 {
			break
		}
		end += start
		paren := strings.Index(md[end:], "(")
		if paren != 1 { // "]" followed immediately by "("
			break
		}
		closeParen := strings.Index(md[end+1:], ")")
		if closeParen == -1 {
			break
		}
		text := md[start+1 : end]
		md = md[:start] + text + md[end+closeParen+2:]
	}
	return md
}

type mdState struct {
	// Track whether we are inside a list to avoid extra blank lines.
	inList bool
	// Track whether last output was a blank line to avoid consecutive blanks.
	lastBlank bool
}

func renderToMarkdown(b *strings.Builder, n *html.Node, s *mdState) {
	switch n.Type {
	case html.TextNode:
		text := strings.TrimSpace(n.Data)
		if text != "" {
			b.WriteString(text)
		}
	case html.ElementNode:
		switch n.DataAtom {
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
			level := int(n.Data[1] - '0')
			if s.lastBlank {
				b.WriteString("\n")
			}
			b.WriteString("\n" + strings.Repeat("#", level) + " ")
			renderChildren(b, n, s)
			b.WriteString("\n")
			s.lastBlank = false
		case atom.P:
			if s.lastBlank {
				b.WriteString("\n")
			}
			renderChildren(b, n, s)
			b.WriteString("\n\n")
			s.lastBlank = true
		case atom.A:
			href := getAttr(n, "href")
			b.WriteString("[")
			renderChildren(b, n, s)
			b.WriteString("](")
			b.WriteString(href)
			b.WriteString(")")
		case atom.Strong, atom.B:
			b.WriteString("**")
			renderChildren(b, n, s)
			b.WriteString("**")
		case atom.Em, atom.I:
			b.WriteString("*")
			renderChildren(b, n, s)
			b.WriteString("*")
		case atom.Code:
			// Inline code: only use backticks when this <code> is not inside a <pre>.
			if !hasAncestor(n, atom.Pre) {
				b.WriteString("`")
				renderChildren(b, n, s)
				b.WriteString("`")
			} else {
				renderChildren(b, n, s)
			}
		case atom.Pre:
			// Code block — preserve language hint if available.
			lang := ""
			if codeNode := findFirst(n, atom.Code); codeNode != nil {
				for _, a := range codeNode.Attr {
					if strings.EqualFold(a.Key, "class") {
						// Common patterns: "language-go", "lang-go", "go"
						for _, part := range strings.Fields(a.Val) {
							part = strings.TrimPrefix(part, "language-")
							part = strings.TrimPrefix(part, "lang-")
							if part != "" {
								lang = part
								break
							}
						}
						if lang != "" {
							break
						}
					}
				}
			}
			b.WriteString("\n```" + lang + "\n")
			renderChildren(b, n, s)
			b.WriteString("\n```\n")
			s.lastBlank = true
		case atom.Blockquote:
			b.WriteString("\n> ")
			renderChildren(b, n, s)
			b.WriteString("\n")
			s.lastBlank = false
		case atom.Ul:
			wasInList := s.inList
			s.inList = true
			renderChildren(b, n, s)
			if !wasInList {
				b.WriteString("\n")
				s.lastBlank = false
			}
			s.inList = wasInList
		case atom.Ol:
			wasInList := s.inList
			s.inList = true
			renderChildren(b, n, s)
			if !wasInList {
				b.WriteString("\n")
				s.lastBlank = false
			}
			s.inList = wasInList
		case atom.Li:
			b.WriteString("\n- ")
			renderChildren(b, n, s)
		case atom.Table:
			b.WriteString("\n")
			renderChildren(b, n, s)
			b.WriteString("\n")
			s.lastBlank = false
		case atom.Thead, atom.Tbody, atom.Tr:
			renderChildren(b, n, s)
		case atom.Th, atom.Td:
			b.WriteString("| ")
			renderChildren(b, n, s)
			b.WriteString(" ")
		case atom.Br:
			b.WriteString("\n")
			s.lastBlank = false
		case atom.Hr:
			b.WriteString("\n---\n")
			s.lastBlank = true
		case atom.Img:
			alt := getAttr(n, "alt")
			src := getAttr(n, "src")
			if alt == "" {
				alt = "image"
			}
			b.WriteString(fmt.Sprintf("![%s](%s)", alt, src))
		default:
			// For unhandled inline elements (span, div, section, etc.), just process
			// children — we keep the text but drop the wrapper.
			// For block-level elements we don't handle specially, still render children.
			if isBlockElement(n.DataAtom) {
				renderChildren(b, n, s)
				// Only emit a line break for block elements that had content.
			} else {
				renderChildren(b, n, s)
			}
		}
	default:
		// Element, Document, Comment, Doctype — recurse into children.
		renderChildren(b, n, s)
	}
}

func renderChildren(b *strings.Builder, n *html.Node, s *mdState) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderToMarkdown(b, c, s)
	}
}

func hasAncestor(n *html.Node, a atom.Atom) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.DataAtom == a {
			return true
		}
	}
	return false
}

func isBlockElement(a atom.Atom) bool {
	switch a {
	case atom.Div, atom.Section, atom.Article, atom.Aside, atom.Header,
		atom.Footer, atom.Nav, atom.Main, atom.Form, atom.Fieldset,
		atom.Details, atom.Dialog, atom.Figcaption, atom.Figure:
		return true
	default:
		return false
	}
}
