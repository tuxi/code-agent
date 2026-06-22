package goal

import (
	"hash/fnv"
	"strconv"
	"strings"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

// Transcript exposes ONLY what the judge is allowed to see: the verifiable
// evidence the worker surfaced — never the full conversation, never a command
// the judge runs itself.
type Transcript interface {
	Evidence() string
}

// EvidenceTools is the whitelist of which tools' results count as verifiable
// evidence. These are real tool names from internal/tools:
//   - run_command: command execution with structured stdout/stderr/exit_code
//   - git_diff:    what files actually changed (needed to judge constraints like
//     "don't modify the test files")
//   - job_logs:    output of a backgrounded long-running command (e.g. a slow
//     test suite started as a job)
//
// A one-line Observation summary is deliberately NOT used: it is lossy and is
// exactly where the worker's self-narration could leak into the judge's input.
var EvidenceTools = map[string]bool{
	"run_command": true,
	"git_diff":    true,
	"job_logs":    true,
}

const (
	defaultMaxEvidenceBytes = 6000 // total cap across all picked results
	defaultPerResultBytes   = 1500 // tail cap per individual result
)

// sessionTranscript projects evidence LIVE from sess.Messages at check time
// (Phase 1: zero new write path — §11.4). The contract is fixed: "the most
// recent whitelisted tool results, each tail-truncated." Phase 3 may swap the
// CAPTURE (emit-time snapshot into a goal-owned buffer) to survive compaction,
// WITHOUT changing this contract or signature.
type sessionTranscript struct {
	sess     *session.Session
	tools    map[string]bool
	maxBytes int
	perBytes int
}

// NewTranscript builds the default read-live evidence view over a session.
func NewTranscript(sess *session.Session) Transcript {
	return &sessionTranscript{
		sess:     sess,
		tools:    EvidenceTools,
		maxBytes: defaultMaxEvidenceBytes,
		perBytes: defaultPerResultBytes,
	}
}

func (t *sessionTranscript) Evidence() string {
	// RoleTool messages do not carry the tool name — only a ToolCallID binding
	// them to the assistant message's ToolCalls. So first index ToolCallID→name,
	// then keep the tool-result messages whose originating tool is whitelisted.
	name := map[string]string{}
	for _, m := range t.sess.Messages {
		for _, tc := range m.ToolCalls {
			name[tc.ID] = tc.Function.Name
		}
	}

	var picked []string
	for _, m := range t.sess.Messages {
		if m.Role != model.RoleTool {
			continue
		}
		if !t.tools[name[m.ToolCallID]] {
			continue
		}
		picked = append(picked, "["+name[m.ToolCallID]+"]\n"+tail(m.Content, t.perBytes))
	}

	// Most-recent-first, accumulated until the total byte budget is reached.
	var out string
	for i := len(picked) - 1; i >= 0; i-- {
		if out != "" && len(out)+len(picked[i])+2 > t.maxBytes {
			break
		}
		if out == "" {
			out = picked[i]
		} else {
			out = picked[i] + "\n\n" + out
		}
	}
	return out
}

// tail keeps the last n bytes of s (the relevant end of a long log), marking the
// elision so the judge knows it is a tail, not the whole output.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// fingerprint hashes the current evidence so the loop can detect "no progress"
// (identical evidence across turns) and stop instead of burning the whole budget.
func fingerprint(evidence string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.TrimSpace(evidence)))
	return strconv.FormatUint(h.Sum64(), 16)
}
