package mcpbridge

import (
	"bytes"
	"code-agent/internal/executionevidence"
	"code-agent/internal/processexec"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxGitOutput = 256 << 10

type cappedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxGitOutput - b.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func gitOutput(ctx context.Context, root string, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cleanup := processexec.Configure(cmd)
	defer cleanup()
	var output, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		isDiff := len(args) > 0 && args[0] == "diff" && contains(args, "--no-index")
		if !(isDiff && errors.As(err, &exit) && exit.ExitCode() == 1) {
			return "", false, fmt.Errorf("git %s failed: %w", args[0], err)
		}
	}
	return output.buffer.String(), output.truncated, nil
}
func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// CommandEvidence is a recorded foreground command result, not a model claim.
// ExitCode is a pointer so absent results/background jobs cannot become PASS.
type CommandEvidence struct {
	Status     string `json:"status,omitempty"`
	CallID     string `json:"call_id"`
	SourceSeq  int64  `json:"source_seq,omitempty"`
	Command    string `json:"command"`
	ExitCode   *int   `json:"exit_code"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Decision   string `json:"decision"`
	Note       string `json:"note,omitempty"`
}

type Artifacts struct {
	ModelCalls        []ModelCallEvidence       `json:"model_calls"`
	TaskAcceptance    string                    `json:"task_acceptance"`
	ExecutionEvidence *executionevidence.Report `json:"execution_evidence,omitempty"`
	EvidenceNote      string                    `json:"evidence_note,omitempty"`
	Experimental      bool                      `json:"experimental"`
	SessionID         string                    `json:"session_id"`
	TurnID            string                    `json:"turn_id"`
	TurnStatus        string                    `json:"turn_status"`
	Baseline          string                    `json:"baseline_commit"`
	Diff              string                    `json:"diff"`
	DiffSHA256        string                    `json:"diff_sha256"`
	Truncated         bool                      `json:"truncated"`
	CommandResults    []CommandEvidence         `json:"command_results"`
	UnparsedResults   []string                  `json:"unparsed_command_call_ids"`
	Note              string                    `json:"note"`
}

func (c *Client) Artifacts(ctx context.Context, id, turnID string) (Artifacts, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.artifacts(ctx, id, turnID)
}

func (c *Client) artifacts(ctx context.Context, id, turnID string) (Artifacts, error) {
	if err := c.checkSession(id); err != nil {
		return Artifacts{}, err
	}
	if c.reattached {
		events, err := c.events(ctx, id, 0)
		if err != nil {
			return Artifacts{}, err
		}
		if err = c.recoverAdmissions(id, events); err != nil {
			return Artifacts{}, err
		}
	}
	r, err := c.Read(ctx, id, turnID, 0)
	if err != nil {
		return Artifacts{}, err
	}
	if !terminal(r.Status) {
		return Artifacts{}, errors.New("wait for the exact turn to finish before collecting artifacts")
	}
	for _, s := range c.submissions {
		if s.receipt == nil {
			return Artifacts{}, errors.New("submission outcome unknown; cannot collect stable evidence")
		}
		state, err := c.Read(ctx, id, s.receipt.TurnID, 0)
		if err != nil {
			return Artifacts{}, err
		}
		if !terminal(state.Status) {
			return Artifacts{}, errors.New("another turn is active; stop writers before review")
		}
	}
	head, _, err := gitOutput(ctx, c.root, "rev-parse", "HEAD")
	if err != nil {
		return Artifacts{}, err
	}
	if strings.TrimSpace(head) != c.baseline {
		return Artifacts{}, errors.New("HEAD changed during experiment; baseline attribution is invalid")
	}
	diff, truncated, err := gitOutput(ctx, c.root, "diff", "--no-ext-diff", "--no-textconv", c.baseline, "--")
	if err != nil {
		return Artifacts{}, err
	}
	untracked, cut, err := gitOutput(ctx, c.root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Artifacts{}, err
	}
	truncated = truncated || cut
	for _, path := range strings.Split(untracked, "\x00") {
		if path == "" {
			continue
		}
		if len(diff) >= maxGitOutput {
			truncated = true
			break
		}
		info, err := os.Lstat(filepath.Join(c.root, path))
		if err != nil || !info.Mode().IsRegular() {
			truncated = true
			continue
		}
		patch, cut, err := gitOutput(ctx, c.root, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--", "/dev/null", path)
		if err != nil {
			return Artifacts{}, err
		}
		truncated = truncated || cut
		diff += patch
	}
	if len(diff) > maxGitOutput {
		diff = diff[:maxGitOutput]
		truncated = true
	}
	hash := sha256.Sum256([]byte(diff))
	a := Artifacts{ModelCalls: []ModelCallEvidence{}, TaskAcceptance: "not_reviewed", ExecutionEvidence: r.ExecutionEvidence, EvidenceNote: r.EvidenceNote, Experimental: true, SessionID: id, TurnID: turnID, TurnStatus: r.Status, Baseline: c.baseline, Diff: diff, DiffSHA256: hex.EncodeToString(hash[:]), Truncated: truncated,
		CommandResults: []CommandEvidence{}, UnparsedResults: []string{}, Note: "P0-A current workspace diff, not an immutable or per-turn snapshot. Stop writers before review. Inspect command exit codes; no test PASS is inferred."}
	for _, e := range r.Events {
		if e.Kind == "model_finished" {
			a.ModelCalls = append(a.ModelCalls, e.ModelCallEvidence)
		}
		if e.Kind != "tool_finished" || e.ToolName != "run_command" {
			continue
		}
		var command CommandEvidence
		observation := e.Observation
		if strings.HasPrefix(observation, "[observation] ") {
			if _, body, ok := strings.Cut(observation, "\n---\n"); ok {
				observation = body
			}
		}
		raw := []byte(observation)
		if e.Output != nil {
			raw, _ = json.Marshal(e.Output)
		}
		if json.Unmarshal(raw, &command) != nil || command.Command == "" || command.ExitCode == nil {
			a.UnparsedResults = append(a.UnparsedResults, e.CallID)
			continue
		}
		command.CallID = e.CallID
		command.SourceSeq = e.Seq
		a.CommandResults = append(a.CommandResults, command)
	}
	return a, nil
}
