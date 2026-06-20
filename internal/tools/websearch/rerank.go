package websearch

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// rerank scores and reorders search results by authority, freshness, and query
// relevance — all deterministic signals. The goal is to push official docs and
// source repos to the top while demoting SEO spam and outdated content.
func rerank(query string, results []Result) []Result {
	if len(results) <= 1 {
		return results
	}

	queryTokens := tokenize(query)
	scored := make([]scoredResult, len(results))
	for i, r := range results {
		scored[i] = scoredResult{
			Result: r,
			score:  scoreResult(query, queryTokens, r),
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	out := make([]Result, len(scored))
	for i, s := range scored {
		out[i] = s.Result
	}
	return out
}

type scoredResult struct {
	Result
	score float64
}

func scoreResult(query string, queryTokens []string, r Result) float64 {
	domainScore := scoreDomain(r.URL)
	freshness := scoreFreshness(r.URL, r.Snippet)
	relevance := scoreRelevance(queryTokens, r.Title, r.Snippet)
	return domainScore*10 + freshness*3 + relevance*4
}

// ── domain authority ───────────────────────────────────────────────────────

func scoreDomain(rawURL string) float64 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 2 // unclassified
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimPrefix(host, "www.")

	// Tier 0: known SEO spam — return early.
	if isSpamDomain(host) {
		return 0
	}

	// Tier 1 (10): source repos and official project sites.
	if isTier1(host) {
		return 10
	}

	// Tier 2 (9): official documentation.
	if isTier2(host) {
		return 9
	}

	// Tier 3 (8): Q&A, knowledge bases.
	if isTier3(host) {
		return 8
	}

	// Tier 4 (6): community blogs and forums.
	if isTier4(host) {
		return 6
	}

	// Tier 5 (4): known tech blogs.
	if isTier5(host) {
		return 4
	}

	// Tier 6 (2): everything else.
	return 2
}

func isTier1(host string) bool {
	switch {
	case host == "github.com":
		return true
	case host == "gitlab.com":
		return true
	case strings.HasSuffix(host, ".github.io"):
		return true
	case host == "bitbucket.org":
		return true
	case host == "codeberg.org":
		return true
	}
	return false
}

func isTier2(host string) bool {
	// Known documentation hosts.
	switch host {
	case "pkg.go.dev", "docs.rs", "crates.io", "npmjs.com", "pypi.org",
		"readthedocs.io", "devdocs.io":
		return true
	}
	// Pattern: docs.<project>.<tld> or developer.<org>.<tld>
	if strings.HasPrefix(host, "docs.") {
		return true
	}
	if strings.HasPrefix(host, "developer.") {
		return true
	}
	if strings.HasPrefix(host, "developers.") {
		return true
	}
	if strings.Contains(host, ".readthedocs.") {
		return true
	}
	// Major vendor doc sites.
	vendorDocs := []string{
		"apple.com", "microsoft.com", "learn.microsoft.com",
		"cloud.google.com", "docs.aws.amazon.com", "kubernetes.io",
		"golang.org", "rust-lang.org", "python.org", "nodejs.org",
		"react.dev", "nextjs.org", "vuejs.org", "angular.io",
		"tailwindcss.com", "svelte.dev", "flutter.dev", "dart.dev",
		"swift.org", "llvm.org", "docker.com", "terraform.io",
		"ansible.com", "nginx.com", "postgresql.org", "mysql.com",
		"redis.io", "mongodb.com", "elastic.co", "kafka.apache.org",
		"nginx.org", "haproxy.org", "grpc.io",
	}
	for _, d := range vendorDocs {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func isTier3(host string) bool {
	switch {
	case host == "stackoverflow.com":
		return true
	case strings.HasSuffix(host, ".stackexchange.com"):
		return true
	case host == "serverfault.com":
		return true
	case host == "superuser.com":
		return true
	case host == "askubuntu.com":
		return true
	}
	return false
}

func isTier4(host string) bool {
	switch {
	case host == "reddit.com" || strings.HasSuffix(host, ".reddit.com"):
		return true
	case host == "news.ycombinator.com":
		return true
	case host == "medium.com":
		return true
	case host == "dev.to":
		return true
	case host == "lobste.rs":
		return true
	}
	return false
}

func isTier5(host string) bool {
	if strings.HasSuffix(host, ".hashnode.dev") ||
		strings.HasSuffix(host, ".substack.com") ||
		strings.HasSuffix(host, ".blogspot.com") ||
		strings.HasSuffix(host, ".wordpress.com") {
		return true
	}
	return false
}

var spamPatterns = []string{
	"spam", "seo", "linkfarm", "clickbank", "traffic",
	"backlink", "content-farm", "scraper", "scraping",
	"aggregator", "article-spinner",
}

func isSpamDomain(host string) bool {
	lower := strings.ToLower(host)
	for _, p := range spamPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ── freshness signal ───────────────────────────────────────────────────────

var yearInURL = regexp.MustCompile(`/(\d{4})/`)

func scoreFreshness(rawURL, _ string) float64 {
	score := 0.0

	// Check URL for year path segments.
	matches := yearInURL.FindStringSubmatch(rawURL)
	if len(matches) > 1 {
		switch matches[1] {
		case "2026":
			score += 3
		case "2025":
			score += 3
		case "2024":
			score += 2
		case "2023":
			score += 1
		}
	}

	// Check snippet for date signals. (snippet param available for future use)
	// For now, URL year is the primary freshness signal.

	return score
}

// ── snippet relevance ──────────────────────────────────────────────────────

func scoreRelevance(queryTokens []string, title, snippet string) float64 {
	if len(queryTokens) == 0 {
		return 0
	}

	combined := strings.ToLower(title + " " + snippet)
	combinedTokens := tokenize(combined)

	// Jaccard-like overlap.
	hitCount := 0
	for _, qt := range queryTokens {
		for _, ct := range combinedTokens {
			if qt == ct {
				hitCount++
				break
			}
		}
	}

	overlap := float64(hitCount) / float64(len(queryTokens))

	// Scale to 0–5.
	score := overlap * 5

	// Bonus: exact phrase match.
	if strings.Contains(strings.ToLower(combined), strings.ToLower(strings.Join(queryTokens, " "))) {
		score += 1
	}

	// Bonus: title match.
	titleLower := strings.ToLower(title)
	for _, qt := range queryTokens {
		if strings.Contains(titleLower, qt) {
			score += 0.5
			break
		}
	}

	// Cap at 5.
	if score > 5 {
		score = 5
	}
	return score
}

// ── tokenizer ───────────────────────────────────────────────────────────────

// tokenize splits text into lowercase alphanumeric tokens, dropping stop words
// and very short tokens.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !isTokenChar(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 2 && !isStopWord(f) {
			out = append(out, f)
		}
	}
	return out
}

func isTokenChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '#' || r == '.'
}
