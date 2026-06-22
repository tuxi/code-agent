package goal

import (
	"context"
	"errors"
	"testing"

	"code-agent/internal/model"
)

// fakeProvider returns canned content/err for one Complete call.
type fakeProvider struct {
	content string
	err     error
}

func (f fakeProvider) Complete(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{Content: f.content}, f.err
}

func TestParseAdmitJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantFuz bool
	}{
		{"verifiable", `{"ok":true,"fuzzy":false,"reason":"go test 通过"}`, true, false},
		{"fuzzy admit", `{"ok":true,"fuzzy":true,"reason":"只能凭证据判断"}`, true, true},
		{"rejected", `{"ok":false,"fuzzy":false,"reason":"无可验证终点"}`, false, false},
		{"fenced", "```json\n{\"ok\":false,\"fuzzy\":false,\"reason\":\"赚钱\"}\n```", false, false},
		{"unparseable fails OPEN", `I think this is fine`, true, true},
		{"empty fails OPEN", ``, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseAdmitJSON(c.raw)
			if got.OK != c.wantOK || got.Fuzzy != c.wantFuz {
				t.Errorf("parseAdmitJSON(%q) = {OK:%v Fuzzy:%v}, want {OK:%v Fuzzy:%v}",
					c.raw, got.OK, got.Fuzzy, c.wantOK, c.wantFuz)
			}
		})
	}
}

func TestLLMAdmitterRoundTrip(t *testing.T) {
	a := &LLMAdmitter{Provider: fakeProvider{content: `{"ok":false,"fuzzy":false,"reason":"帮我赚钱:没有可机检终点"}`}}
	res, err := a.Admit(context.Background(), "帮我赚钱")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Errorf("want rejected, got OK with reason %q", res.Reason)
	}
}

// A provider error surfaces to the caller (the driver decides to fail open).
func TestLLMAdmitterProviderError(t *testing.T) {
	a := &LLMAdmitter{Provider: fakeProvider{err: errors.New("boom")}}
	if _, err := a.Admit(context.Background(), "x"); err == nil {
		t.Error("want provider error to surface, got nil")
	}
}
