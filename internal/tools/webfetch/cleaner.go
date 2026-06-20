package webfetch

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// cleanHTML extracts the main content body from an HTML document. It removes
// boilerplate (header, footer, nav, scripts, styles) and advertising/cookie
// elements, then returns a cleaned <body>-equivalent subtree for further
// conversion. The input is an *html.Node from golang.org/x/net/html.
func cleanHTML(doc *html.Node) *html.Node {
	body := findBody(doc)
	if body == nil {
		return doc // fallback to full document
	}

	// 1. Try semantic elements first.
	if n := findFirst(body, atom.Article); n != nil {
		return cloneTree(n)
	}
	if n := findFirst(body, atom.Main); n != nil {
		return cloneTree(n)
	}

	// 2. Remove boilerplate from body, then return what remains.
	clean := cloneTree(body)
	removeBoilerplate(clean)
	return clean
}

// findBody returns the <body> element, or nil.
func findBody(n *html.Node) *html.Node {
	return findFirst(n, atom.Body)
}

// findFirst does a depth-first search for the first element with the given atom.
func findFirst(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findFirst(c, a); found != nil {
			return found
		}
	}
	return nil
}

// cloneTree returns a deep copy of n.
func cloneTree(n *html.Node) *html.Node {
	clone := &html.Node{
		Type:     n.Type,
		DataAtom: n.DataAtom,
		Data:     n.Data,
		Attr:     make([]html.Attribute, len(n.Attr)),
	}
	copy(clone.Attr, n.Attr)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		clone.AppendChild(cloneTree(c))
	}
	return clone
}

// removeBoilerplate strips header, footer, nav, script, style, and common
// ad/cookie elements from the tree. It is called recursively.
func removeBoilerplate(n *html.Node) {
	var remove []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.Header, atom.Footer, atom.Nav, atom.Script, atom.Style, atom.Noscript:
			remove = append(remove, c)
		case atom.Aside:
			// Keep <aside> only if it doesn't look like an ad.
			if hasAdClass(c) || hasRole(c, "complementary") {
				remove = append(remove, c)
			}
		default:
			// Remove elements that look like ads or cookie banners.
			if hasAdClass(c) || hasCookieClass(c) || hasRole(c, "banner") {
				remove = append(remove, c)
			}
		}
	}
	for _, c := range remove {
		n.RemoveChild(c)
	}
	// Recurse into remaining children.
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		removeBoilerplate(c)
	}
}

func hasAdClass(n *html.Node) bool {
	return hasClassContaining(n, "ad", "advertisement", "advert", "sponsored", "promo", "banner-ad")
}

func hasCookieClass(n *html.Node) bool {
	return hasClassContaining(n, "cookie", "consent", "gdpr", "ccpa")
}

func hasClassContaining(n *html.Node, substrs ...string) bool {
	class := getAttr(n, "class")
	if class == "" {
		return false
	}
	lower := strings.ToLower(class)
	for _, s := range substrs {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func hasRole(n *html.Node, role string) bool {
	return strings.EqualFold(getAttr(n, "role"), role)
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// extractLinks collects all href attributes from <a> elements in the tree,
// deduplicated and in document order. Used to populate the "links" field of the
// web_fetch output.
func extractLinks(n *html.Node) []string {
	seen := make(map[string]bool)
	var links []string
	walk(n, func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.A {
			href := strings.TrimSpace(getAttr(n, "href"))
			if href != "" && !strings.HasPrefix(href, "#") && !strings.HasPrefix(href, "javascript:") {
				if !seen[href] {
					seen[href] = true
					links = append(links, href)
				}
			}
		}
	})
	return links
}

func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// extractTitle returns the first <title> text content.
func extractTitle(doc *html.Node) string {
	if titleNode := findFirst(doc, atom.Title); titleNode != nil {
		return strings.TrimSpace(textContent(titleNode))
	}
	// Fallback: first <h1>.
	if h1 := findFirst(doc, atom.H1); h1 != nil {
		return strings.TrimSpace(textContent(h1))
	}
	return ""
}

func textContent(n *html.Node) string {
	var b strings.Builder
	walk(n, func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
	})
	return b.String()
}
