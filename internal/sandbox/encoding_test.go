package sandbox

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The rewritten command from ConvertBackticks is exactly what run_command
// executes, so every byte outside a converted `...` span must survive verbatim.
// Regressing to a byte-as-rune scan here double-encodes CJK text: each UTF-8
// byte becomes a Latin-1 rune and is written back as two bytes, so a Python/sed
// literal no longer matches the file it was copied from.
func TestConvertBackticksPreservesNonASCII(t *testing.T) {
	tests := []struct{ name, command, want string }{
		{"no backtick, CJK untouched", "echo 中文 参数", "echo 中文 参数"},
		{"CJK around a substitution", "echo 中文 `date` 结束", "echo 中文 $(date) 结束"},
		{"CJK inside a substitution", "echo `配置库` 完成", "echo $(配置库) 完成"},
		{"emoji", "echo 😀 `whoami`", "echo 😀 $(whoami)"},
		{"CJK in single quotes is not expanded", "git commit -m 'a `b` 中文'", "git commit -m 'a `b` 中文'"},
		{"unterminated backtick keeps the tail", "echo 未闭合 `中文", "echo 未闭合 `中文"},
		{
			"the transcript case: CJK literal with backticks",
			"python3 - <<'PY'\nold = '该 stub 的 `MCProfileIsRemovalStub=true`'\nPY",
			"python3 - <<'PY'\nold = '该 stub 的 `MCProfileIsRemovalStub=true`'\nPY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertBackticks(tt.command)
			if got != tt.want {
				t.Errorf("ConvertBackticks(%q)\n got %q\nwant %q", tt.command, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("ConvertBackticks(%q) produced invalid UTF-8: % x", tt.command, []byte(got))
			}
		})
	}
}

// Splitting the rewrite into "replace `x` with $(x), copy everything else" must
// be byte-exact even for input that is not valid UTF-8: mangling arbitrary bytes
// is never better than passing them through.
func TestConvertBackticksIsByteExactOutsideSubstitutions(t *testing.T) {
	inputs := []string{
		"echo 中文",
		"echo 中文 `date` 结束",
		"echo `中文` `日本語` `한국어`",
		"echo \xff\xfe `x` \xff",
	}
	for _, in := range inputs {
		got := ConvertBackticks(in)
		// Removing the injected "$(" and ")" characters must give back the input.
		stripped := strings.NewReplacer("$(", "", ")", "").Replace(got)
		want := strings.ReplaceAll(in, "`", "")
		if stripped != want {
			t.Errorf("ConvertBackticks(%q) = %q: non-substitution bytes changed (got %q, want %q)", in, got, stripped, want)
		}
	}
}

func TestConvertBackticksNoBacktickIsIdentity(t *testing.T) {
	for _, in := range []string{"", "echo 中文", "go build ./... && 中文"} {
		if got := ConvertBackticks(in); got != in {
			t.Errorf("ConvertBackticks(%q) = %q, want identity", in, got)
		}
	}
}

// stripSubstitutions feeds its result back into Classify, so a re-encoding bug
// there would classify a mangled command — the wrong permission decision for a
// command the runtime then executes as written.
func TestStripSubstitutionsPreservesNonASCII(t *testing.T) {
	tests := []struct{ command, want string }{
		{"echo 中文 $(date) 结束", "echo 中文 '' 结束"},
		{"grep '中文' $(ls) 文件", "grep '中文' '' 文件"},
		{"cat \"中文 $(pwd) 路径\"", "cat \"中文 '' 路径\""},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := stripSubstitutions(tt.command)
			if got != tt.want {
				t.Errorf("stripSubstitutions(%q) = %q, want %q", tt.command, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("stripSubstitutions(%q) produced invalid UTF-8: % x", tt.command, []byte(got))
			}
		})
	}
}
