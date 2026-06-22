package goal

import (
	"context"
	"encoding/json"

	"code-agent/internal/model"
)

// AdmitResult is the set-time verdict on whether an objective belongs in /goal (§4.7).
type AdmitResult struct {
	// OK: the objective has a machine-checkable or evidence-judgeable endpoint and
	// is not a high-risk real-world action.
	OK bool `json:"ok"`
	// Fuzzy: admitted, but judgeable only by evidence (no clean verify command), so
	// the judge is less reliable than a command-based endpoint. Surfaced as a caveat.
	Fuzzy bool `json:"fuzzy"`
	// Reason: why; on rejection, what makes it unfit and how to rephrase it.
	Reason string `json:"reason"`
}

// Admitter judges, at set time, whether an objective is fit for /goal. It is the
// gate behind acceptance criterion §8.2 (reject "make money"-type goals).
type Admitter interface {
	Admit(ctx context.Context, objective string) (AdmitResult, error)
}

// LLMAdmitter classifies the objective with a cheap, independent model — same
// posture as the judge (separate model, strict JSON), except it reads ONLY the
// objective: at set time there is no evidence yet.
type LLMAdmitter struct {
	Provider model.Provider
	Model    string
}

const admitterSystem = `你是 /goal 目标准入判定器。/goal 适合这样的目标:有客观、可机检的终点` +
	`(测试通过 / build 成功 / 某命令退出码为 0 / 覆盖率达阈值 / eval 分数达标等),逼近过程是「改一版、验一版」的机械迭代。
判定给定目标是否适合 /goal,只输出严格 JSON:{"ok":bool,"fuzzy":bool,"reason":string},不要任何额外文字。
- ok=true:能为它写出一条 verify 命令,或一个可由证据机检的判定。
  - fuzzy=false:有干净的命令式终点(如「go test 通过」)。
  - fuzzy=true:只能由证据大致判定、没有干净命令(裁判可靠性较低)。
- ok=false:没有任何可验证终点(如「把代码写优雅」「帮我赚钱」「完善一下功能」),
  或需要执行高风险真实动作(动账户 / 收付款 / 删除数据 / 对外发布内容)。
reason 用中文简述理由;ok=false 时必须说明为何不适合,并建议如何改写成一个可验证的目标。
信息不足以判断时从宽 ok=true(让循环去尝试),但置 fuzzy=true。`

func (a *LLMAdmitter) Admit(ctx context.Context, objective string) (AdmitResult, error) {
	resp, err := a.Provider.Complete(ctx, model.Request{
		Model:       a.Model,
		Temperature: 0,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: admitterSystem},
			{Role: model.RoleUser, Content: "目标:\n" + objective},
		},
	})
	if err != nil {
		return AdmitResult{}, err
	}
	return parseAdmitJSON(resp.Content), nil
}

// parseAdmitJSON tolerates fences/prose and FAILS OPEN (ok=true, fuzzy=true) on
// unparseable output. Admission is an advisory UX gate, not the safety boundary —
// the approver guards high-risk ACTIONS regardless — so a flaky classifier must
// not block legitimate work; it only loses the upfront warning.
func parseAdmitJSON(raw string) AdmitResult {
	var r AdmitResult
	if err := json.Unmarshal([]byte(carveJSON(raw)), &r); err != nil {
		return AdmitResult{OK: true, Fuzzy: true, Reason: "准入判定输出无法解析,从宽放行"}
	}
	return r
}
