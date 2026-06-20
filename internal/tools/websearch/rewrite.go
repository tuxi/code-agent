package websearch

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Rewriter transforms a conversational user query into a search-engine-optimized
// query. The primary path is a fast heuristic (no model call); an optional LLM
// fallback fires only when heuristic confidence is low.
type Rewriter struct {
	// LLMFallback, if set, is called when the heuristic confidence is below
	// threshold. The original query is passed; the returned string replaces the
	// heuristic result. If the call errors or times out, the heuristic result is
	// used silently.
	LLMFallback func(ctx context.Context, query string) (string, error)
}

// Rewrite returns a search-optimized query. It always runs the heuristic first;
// the LLM fallback is only consulted when the heuristic confidence is low AND a
// fallback function is configured.
func (r *Rewriter) Rewrite(ctx context.Context, query string) string {
	rewritten, confidence := heuristicRewrite(query)
	if confidence < 0.4 && r.LLMFallback != nil {
		if improved, err := r.LLMFallback(ctx, query); err == nil && improved != "" {
			return strings.TrimSpace(improved)
		}
	}
	return rewritten
}

// ── heuristic rewriter ──────────────────────────────────────────────────────

// fillerPatterns are conversational phrases stripped from the query.
var fillerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(can you tell me|can you explain|can you show me|can you help me with|can you find|i want to know|i want to understand|i need to|i'd like to|i would like to|please explain|please tell me|please show me|tell me|show me|explain to me|what is the best way to|how does|how is|how are|how do i|how to|how do you)\b`),
	regexp.MustCompile(`(?i)(请帮我查一下|帮我查一下|请帮我|帮我|我想知道|我想了解|请问|你能帮我|你能|查一下)`),
}

// intentPatterns maps query intent to injected technical keywords.
type intentPattern struct {
	pattern *regexp.Regexp
	suffix  string
}

var intents = []intentPattern{
	// "how does X work" / "how is X implemented"
	{regexp.MustCompile(`(?i)\bhow\s+(does|is|do|are)\s+.+\s+(work|implement|built|designed|made|constructed|done)\b`),
		"implementation internals design architecture"},
	// "what is X" / "what are X"
	{regexp.MustCompile(`(?i)\bwhat\s+(is|are)\s+`),
		"documentation overview guide"},
	// "X vs Y" / "X versus Y" / "X compared to Y"
	{regexp.MustCompile(`(?i)\b(vs\.?|versus|compared\s+to|compared\s+with|difference\s+between)\b`),
		"comparison differences benchmark"},
	// "X error Y" / "X failed Y" / "X bug Y" / "fix X"
	{regexp.MustCompile(`(?i)\b(error|failed|fails|bug|crash|broken|issue|exception|panic)\b`),
		"fix solution troubleshooting"},
	{regexp.MustCompile(`(?i)\b(fix|solve|resolve|repair|debug)\s+`),
		"fix solution troubleshooting"},
	// "X example" / "X tutorial" / "X code example"
	{regexp.MustCompile(`(?i)\b(example|tutorial|sample|demo|how\s+to)\b`),
		"code example tutorial guide"},
	// "best practice X"
	{regexp.MustCompile(`(?i)\bbest\s+(practice|way|approach|method)\s+`),
		"best practice guide recommendations"},
	// "X performance" / "X slow"
	{regexp.MustCompile(`(?i)\b(performance|slow|fast|speed|optimize|optimization|benchmark|latency|throughput)\b`),
		"performance optimization benchmark"},
	// "X config" / "X setup" / "X configuration"
	{regexp.MustCompile(`(?i)\b(config|configure|setup|set\s+up|install|configuration|deployment|deploy)\b`),
		"setup configuration guide"},
	// "X API" / "X SDK" / "X library"
	{regexp.MustCompile(`(?i)\b(api|sdk|library|package|module|framework|endpoint)\b`),
		"api reference documentation example"},
}

// heuristicRewrite strips conversational filler, detects intent, injects
// technical keywords, and returns the rewritten query along with a confidence
// score in [0, 1].
func heuristicRewrite(query string) (string, float64) {
	original := strings.TrimSpace(query)
	if original == "" {
		return original, 0
	}

	// Step 1: detect intent from the ORIGINAL query BEFORE stripping filler,
	// because the filler words ("how does", "what is") carry the intent signal.
	intentSuffix := ""
	intentMatched := false
	for _, intent := range intents {
		if intent.pattern.MatchString(original) {
			intentSuffix = intent.suffix
			intentMatched = true
			break
		}
	}

	// Step 2: strip conversational filler.
	cleaned := original
	for _, p := range fillerPatterns {
		cleaned = p.ReplaceAllString(cleaned, "")
	}
	cleaned = strings.TrimSpace(cleaned)

	// Step 3: remove leading/trailing question marks and whitespace.
	cleaned = strings.TrimRight(cleaned, "?？!！.。,，")
	cleaned = strings.TrimSpace(cleaned)

	// If stripping removed everything, fall back to original.
	if cleaned == "" {
		cleaned = original
	}

	// Step 3.5: correct stale years. LLMs often hallucinate old years (e.g.
	// "2025" when it's 2026). Replace any standalone 4-digit year that looks
	// stale with the current year. Only kicks in for years within a 5-year
	// window around "now" — historical years ("Python 2.7 released 2010") are
	// left alone.
	cleaned = correctStaleYear(cleaned)

	// Step 4: inject intent keywords.
	if intentSuffix != "" {
		cleaned = cleaned + " " + intentSuffix
	} else if countTechnicalWords(cleaned) >= 3 {
		// No intent pattern matched but query has ≥3 technical words — already a
		// good search query. Mark as matched so confidence stays high.
		intentMatched = true
	} else {
		// Safe fallback.
		cleaned = cleaned + " guide documentation"
	}

	// Step 6: collapse whitespace, remove duplicate words.
	cleaned = collapseWhitespace(cleaned)
	cleaned = dedupWords(cleaned)

	// Compute confidence.
	confidence := computeConfidence(original, cleaned, intentMatched)

	return cleaned, confidence
}

func countTechnicalWords(s string) int {
	words := strings.Fields(s)
	count := 0
	for _, w := range words {
		w = strings.TrimRight(w, ",.;:!?()[]{}")
		if len(w) >= 3 && !isStopWord(w) {
			count++
		}
	}
	return count
}

// isStopWord returns true for common conversational/function words that carry
// no technical signal.
func isStopWord(w string) bool {
	switch strings.ToLower(w) {
	case "the", "a", "an", "is", "are", "was", "were", "be", "been",
		"of", "in", "on", "at", "to", "for", "with", "by", "from",
		"and", "or", "but", "not", "no", "yes", "it", "its", "this",
		"that", "these", "those", "i", "you", "he", "she", "we", "they",
		"my", "your", "his", "her", "our", "their", "me", "him", "us",
		"them", "can", "will", "would", "could", "should", "may", "might",
		"do", "does", "did", "has", "have", "had", "how", "what", "when",
		"where", "which", "who", "why", "very", "just", "then", "than",
		"also", "too", "only", "some", "any", "all", "each", "every",
		"about", "into", "over", "after", "before", "between", "under",
		"here", "there", "now", "still", "really", "actually":
		return true
	}
	return false
}

func collapseWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func dedupWords(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
	})
	seen := make(map[string]bool, len(words))
	var result []string
	for _, w := range words {
		lower := strings.ToLower(w)
		if len(w) < 2 || seen[lower] {
			continue
		}
		seen[lower] = true
		result = append(result, w)
	}

	// Reconstruct: keep the non-word separators from the original.
	// Simpler approach: just join deduped words with spaces.
	return strings.Join(result, " ")
}

// yearPattern matches 4-digit years that stand alone (not inside longer numbers).
var yearPattern = regexp.MustCompile(`\b(20[0-9]{2})\b`)

// correctStaleYear replaces stale 4-digit years with the current year. Only
// years within a 5-year window behind "now" are considered stale — older years
// are treated as intentional historical references and left alone.
func correctStaleYear(query string) string {
	currentYear := time.Now().Year()
	return yearPattern.ReplaceAllStringFunc(query, func(match string) string {
		y, err := strconv.Atoi(match)
		if err != nil {
			return match
		}
		// Stale: in [currentYear-5, currentYear-1] — likely an LLM hallucination.
		if y < currentYear && y >= currentYear-5 {
			return strconv.Itoa(currentYear)
		}
		return match
	})
}

func computeConfidence(original, rewritten string, intentMatched bool) float64 {
	score := 0.5 // neutral baseline

	// Intent match is a strong signal.
	if intentMatched {
		score += 0.3
	}

	// Very short queries are inherently ambiguous.
	wordCount := len(strings.Fields(original))
	if wordCount >= 5 {
		score += 0.15
	} else if wordCount < 3 {
		score -= 0.25
	}

	// Technical word density.
	techWords := countTechnicalWords(original)
	if techWords >= 4 {
		score += 0.1
	}

	// Clamp to [0, 1].
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score
}
