package sandbox

import "strings"

// dangerousTokenPattern describes a sequence of tokens that, when detected in a
// command's argv (in order, not necessarily contiguous), forces a Block decision
// regardless of prefix matching. Unlike the substring-based BlockedCommands, these
// patterns are structure-aware: they operate on SplitArgs output, so word order
// is irrelevant and quoted content is excluded.
//
// An empty token in the pattern acts as a wildcard: it matches any single token
// without consuming it (useful for skipping flags like "origin").
type dangerousTokenPattern struct {
	tokens []string // must appear in this relative order; "" = wildcard
	desc   string   // shown to the model when blocked: "dangerous pattern: <desc>"
}

// dangerousPatterns is the registry of structure-aware dangerous patterns.
// These are checked AFTER the substring-based BlockedCommands but BEFORE the
// allow/confirm prefix match, so a prefix allow rule cannot override them.
//
// To add a pattern:
//   - List the tokens that must appear in order (anywhere in argv)
//   - Use "" for positional wildcards (e.g. skip the remote name)
//   - Keep the desc short — it appears in the Reason field
var dangerousPatterns = []dangerousTokenPattern{
	// ── Force-push to default branch (any word order) ──
	{tokens: []string{"push", "--force", "main"}, desc: "force-push to main branch"},
	{tokens: []string{"push", "-f", "main"}, desc: "force-push to main branch"},
	{tokens: []string{"push", "--force", "master"}, desc: "force-push to master branch"},
	{tokens: []string{"push", "-f", "master"}, desc: "force-push to master branch"},
	{tokens: []string{"push", "--force-with-lease", "main"}, desc: "force-push to main branch"},
	{tokens: []string{"push", "--force-with-lease", "master"}, desc: "force-push to master branch"},
	{tokens: []string{"push", "--delete", "main"}, desc: "delete main branch on remote"},
	{tokens: []string{"push", "--delete", "master"}, desc: "delete master branch on remote"},

	// ── Recursive deletion of workspace/home ──
	{tokens: []string{"rm", "-rf", "."}, desc: "recursive delete of current directory"},
	{tokens: []string{"rm", "-fr", "."}, desc: "recursive delete of current directory"},
	{tokens: []string{"rm", "-r", "-f", "."}, desc: "recursive delete of current directory"},
	{tokens: []string{"rm", "-rf", "~"}, desc: "recursive delete of home directory"},

	// ── Destructive git working-tree operations ──
	{tokens: []string{"git", "reset", "--hard"}, desc: "hard git reset discards all working-tree changes"},
	{tokens: []string{"git", "clean", "-fd"}, desc: "force-delete untracked files and directories"},
	{tokens: []string{"git", "clean", "-fdx"}, desc: "force-delete all untracked files including gitignored"},
	{tokens: []string{"git", "checkout", "--force"}, desc: "force checkout discards working-tree changes"},

	// ── World-writable permission changes ──
	{tokens: []string{"chmod", "-R", "777"}, desc: "recursive world-writable permission change"},
	{tokens: []string{"chmod", "777"}, desc: "world-writable permission change"},

	// ── Curl/wget piping to shell interpreter ──
	// The pipe operator is already rejected by ContainsShellOperators, but these
	// patterns catch the intent even when the model rephrases without a pipe
	// (e.g. curl -o /tmp/x && bash /tmp/x — the && is also rejected, but the
	// pattern still fires as defense-in-depth).
	{tokens: []string{"curl", "|", "bash"}, desc: "downloading and executing remote code via shell pipe"},
	{tokens: []string{"curl", "|", "sh"}, desc: "downloading and executing remote code via shell pipe"},
	{tokens: []string{"wget", "|", "bash"}, desc: "downloading and executing remote code via shell pipe"},
	{tokens: []string{"wget", "|", "sh"}, desc: "downloading and executing remote code via shell pipe"},

	// ── Environment / credential exfiltration ──
	{tokens: []string{"cat", "*.env*", "|"}, desc: "attempt to pipe .env content to another command"},
	{tokens: []string{"cat", "*.env*", ">"}, desc: "attempt to redirect .env content"},
	{tokens: []string{"curl", "*.env*"}, desc: "suspicious .env reference with curl"},
	{tokens: []string{"curl", "*token*"}, desc: "suspicious token reference with curl"},

	// ── eval / source with dynamic content ──
	{tokens: []string{"eval", "*$*"}, desc: "eval with variable expansion"},
	{tokens: []string{"source", "*/dev*"}, desc: "sourcing from /dev"},
}

// matchDangerousTokens checks whether any dangerous pattern matches the given
// argv. It returns the matched pattern and true, or zero-value and false.
func matchDangerousTokens(args []string) (dangerousTokenPattern, bool) {
	for _, dp := range dangerousPatterns {
		if matchTokenSequence(args, dp.tokens) {
			return dp, true
		}
	}
	return dangerousTokenPattern{}, false
}

// matchTokenSequence reports whether all non-empty tokens in pattern appear in
// args in the same relative order (not necessarily contiguous).
//
// An empty pattern token acts as a positional wildcard: it skips exactly one arg
// without matching.
//
// A pattern token wrapped in *stars* (e.g. "*$*") does a case-insensitive
// substring match against each arg. A plain token does an exact case-insensitive
// match.
//
// Examples:
//
//	matchTokenSequence(["git","push","--force","origin","main"], ["push","--force","main"]) → true
//	matchTokenSequence(["git","push","main"], ["push","--force","main"])              → false
//	matchTokenSequence(["eval","$CMD"], ["eval","*$*"])                               → true
func matchTokenSequence(args []string, pattern []string) bool {
	argIdx := 0
	for _, pt := range pattern {
		if pt == "" {
			// Positional wildcard: skip exactly one arg if available.
			if argIdx < len(args) {
				argIdx++
			}
			continue
		}
		// A pattern wrapped in * means substring match; strip the stars.
		isContains := strings.HasPrefix(pt, "*") && strings.HasSuffix(pt, "*")
		search := pt
		if isContains {
			search = pt[1 : len(pt)-1]
		}

		found := false
		for argIdx < len(args) {
			matched := false
			if isContains {
				matched = strings.Contains(strings.ToLower(args[argIdx]), strings.ToLower(search))
			} else {
				matched = strings.EqualFold(args[argIdx], search)
			}
			if matched {
				found = true
				argIdx++
				break
			}
			argIdx++
		}
		if !found {
			return false
		}
	}
	return true
}
