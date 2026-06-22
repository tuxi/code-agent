package goal

import (
	"context"
	"encoding/json"
	"strings"

	"code-agent/internal/model"
)

// CheckResult is the judge's verdict. Blocked means "no viable path, stop and
// ask the user", distinct from "not yet met, keep going".
type CheckResult struct {
	Met     bool   `json:"met"`
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason"`
}

// Checker decides whether the goal is met. It is ALWAYS separate from the worker
// (the worker never grades itself) and never runs commands — it only reads the
// Evidence the worker already surfaced.
type Checker interface {
	Check(ctx context.Context, g *Goal, t Transcript) (CheckResult, error)
}

// LLMChecker is the separated judge (Claude Code form): an independent, cheap
// model that sees only the objective + Evidence. Its Provider MUST be a
// different model from the worker's (built like resolveSubAgentModel, on a
// flash-class model). reason feeds the next worker turn as a gradient.
type LLMChecker struct {
	Provider model.Provider // independent of the worker's provider
	Model    string         // the cheap judge model (e.g. a deepseek small model)
}

const checkerSystem = `你是目标达成评判者。只依据提供的证据判断条件是否满足,证据不足时判未满足。` +
	`不要臆测、不要替工作模型辩护。只输出严格 JSON:{"met":bool,"blocked":bool,"reason":string},不要任何额外文字。`

func (c *LLMChecker) Check(ctx context.Context, g *Goal, t Transcript) (CheckResult, error) {
	resp, err := c.Provider.Complete(ctx, model.Request{
		Model:       c.Model,
		Temperature: 0,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: checkerSystem},
			{Role: model.RoleUser, Content: "目标条件:\n" + g.Objective + "\n\n已观察证据(仅以下内容为准):\n" + t.Evidence()},
		},
	})
	if err != nil {
		return CheckResult{}, err
	}
	return parseCheckJSON(resp.Content), nil
}

// parseCheckJSON tolerates ```json fences and surrounding prose, and DEGRADES to
// "not met" rather than erroring: an unparseable judge must never be read as
// "achieved". Failing safe means continuing the loop, never a false success.
func parseCheckJSON(raw string) CheckResult {
	s := strings.TrimSpace(raw)
	// Carve out the outermost {...} so fences/preamble don't break json.Unmarshal.
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j >= i {
			s = s[i : j+1]
		}
	}
	var r CheckResult
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return CheckResult{Met: false, Reason: "checker output unparseable: " + truncate(raw, 200)}
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
