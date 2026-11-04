package filesystem

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) (root, rel string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, name
}

func runEdit(t *testing.T, root string, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := NewEditFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, raw)
	return res.Content, err
}

func TestEditFileReplacesUnique(t *testing.T) {
	root, rel := writeTemp(t, "f.go", "package x\n\nfunc A() {}\n")
	if _, err := runEdit(t, root, map[string]any{"path": rel, "old": "func A()", "new": "func B()"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if !strings.Contains(string(got), "func B()") || strings.Contains(string(got), "func A()") {
		t.Fatalf("replacement not applied: %s", got)
	}
}

func TestEditFileNotFoundIsRecoverable(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "hello world\n")
	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "goodbye", "new": "hi"})
	if err != nil {
		t.Fatalf("expected a recoverable observation, got error: %v", err)
	}
	if !strings.Contains(content, "Could not find") {
		t.Errorf("expected a not-found message, got: %s", content)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != "hello world\n" {
		t.Errorf("file should be unchanged, got: %s", got)
	}
}

func TestEditFileAmbiguousIsRecoverable(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "x\nx\n")
	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "x", "new": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "appears 2 times") {
		t.Errorf("expected an ambiguity message, got: %s", content)
	}
}

func TestEditFileDeletes(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "keep\nremove me\nkeep\n")
	if _, err := runEdit(t, root, map[string]any{"path": rel, "old": "remove me\n", "new": "", "allow_delete": true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); strings.Contains(string(got), "remove me") {
		t.Errorf("text not deleted: %s", got)
	}
}

// An empty 'new' on a non-blank 'old' is either a deliberate deletion or a
// truncated tool call. It must never be applied silently: the two are
// indistinguishable at the tool boundary, and guessing wrong destroys content.
func TestEditFileRefusesSilentDelete(t *testing.T) {
	const original = "keep\nremove me\nkeep\n"
	root, rel := writeTemp(t, "f.txt", original)

	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "remove me\n", "new": ""})
	if err != nil {
		t.Fatalf("expected a recoverable observation, got error: %v", err)
	}
	if !strings.Contains(content, "allow_delete") {
		t.Errorf("refusal must name the escape hatch, got: %s", content)
	}
	if !strings.Contains(content, "1 line(s)") {
		t.Errorf("refusal must state the damage, got: %s", content)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != original {
		t.Errorf("file must be untouched, got: %q", got)
	}
}

// Removing whitespace only (a blank line) carries no content risk, so it stays
// a plain edit: there is nothing to lose and no reason to demand a flag.
func TestEditFileDeletesBlankLineWithoutFlag(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\n\n\nb\n")
	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "a\n\n", "new": "a\n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "allow_delete") {
		t.Errorf("blank-line removal should not require allow_delete: %s", content)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != "a\n\nb\n" {
		t.Errorf("got %q, want %q", got, "a\n\nb\n")
	}
}

// A deletion says so, instead of reporting an edit whose new text happens to be
// empty — the wording is what the model reads back to confirm what happened.
func TestEditFileDeletionHeader(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "keep\nremove me\nkeep\n")
	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "remove me\n", "new": "", "allow_delete": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(content, "Deleted 1 line(s) at line 2 of") {
		t.Errorf("deletion header not reported: %s", content)
	}
}

// A whole-line edit whose 'old' ends with "\n" must report exactly the touched
// lines: the terminating newline belongs to its own line, so the next line is
// context. Over-counting by one made successful edits read as "the line after
// your edit was deleted and replaced by a blank line".
func TestEditDiffWholeLineAccounting(t *testing.T) {
	const base = "intro\n1. 配置库共 12 个文件\n3. **没有 MDM.plist\noutro\n"

	tests := []struct {
		name        string
		old, new    string
		want        string
		mustNotHave []string
	}{
		{
			// Deleting a whole line: no phantom "+N <blank>" line may appear.
			name: "delete whole line",
			old:  "3. **没有 MDM.plist\n",
			new:  "",
			want: " 2\t1. 配置库共 12 个文件\n" +
				"-3\t3. **没有 MDM.plist\n" +
				" 3\toutro",
			mustNotHave: []string{"-4\toutro", "+3\t"},
		},
		{
			// Inserting after a whole line: the following line is context, not a
			// removal, and the added lines carry no trailing blank.
			name: "insert whole line",
			old:  "1. 配置库共 12 个文件\n",
			new:  "1. 配置库共 12 个文件\n2. 它不是 MDM 描述文件\n",
			want: " 1\tintro\n" +
				"-2\t1. 配置库共 12 个文件\n" +
				"+2\t1. 配置库共 12 个文件\n" +
				"+3\t2. 它不是 MDM 描述文件\n" +
				" 4\t3. **没有 MDM.plist",
			mustNotHave: []string{"-3\t3. **没有", "+4\t"},
		},
		{
			// A replacement that does not end with "\n" still adds its lines only.
			// (The file then glues the next line onto it — that is the edit the
			// model asked for; the diff shows the resulting context faithfully.)
			name: "new without trailing newline",
			old:  "1. 配置库共 12 个文件\n",
			new:  "1. 配置库共 12 个文件\n2. 它不是 MDM 描述文件",
			want: " 1\tintro\n" +
				"-2\t1. 配置库共 12 个文件\n" +
				"+2\t1. 配置库共 12 个文件\n" +
				"+3\t2. 它不是 MDM 描述文件\n" +
				" 4\toutro",
			mustNotHave: []string{"-3\t3. **没有", "+4\t"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := strings.Index(base, tt.old)
			if start < 0 {
				t.Fatalf("fixture broken: %q not in base", tt.old)
			}
			newContent := base[:start] + tt.new + base[start+len(tt.old):]
			got := editDiff(base, newContent, tt.old, tt.new, start, 1)
			if got != tt.want {
				t.Errorf("editDiff:\n got:\n%s\nwant:\n%s", got, tt.want)
			}
			for _, bad := range tt.mustNotHave {
				if strings.Contains(got, bad) {
					t.Errorf("diff must not contain %q:\n%s", bad, got)
				}
			}
		})
	}
}

// The edit really did succeed, so the observation must not imply that content
// was lost — that misreading is what sent the agent chasing an imaginary bug.
func TestEditFileInsertLineObservationIsAccurate(t *testing.T) {
	root, rel := writeTemp(t, "f.md", "intro\n1. 配置库共 12 个文件\n3. **没有 MDM.plist\noutro\n")
	content, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  "1. 配置库共 12 个文件\n",
		"new":  "1. 配置库共 12 个文件\n2. 它不是 MDM 描述文件\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "+3\t2. 它不是 MDM 描述文件") {
		t.Errorf("diff should show the inserted line:\n%s", content)
	}
	if strings.Contains(content, "-3\t3. **没有") {
		t.Errorf("the line after the edit is context, not a removal:\n%s", content)
	}
	if strings.Contains(content, "+4\t") {
		t.Errorf("diff must not report a phantom blank line:\n%s", content)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if want := "intro\n1. 配置库共 12 个文件\n2. 它不是 MDM 描述文件\n3. **没有 MDM.plist\noutro\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestEditDiffFormat(t *testing.T) {
	oldContent := "package p\n\n// old comment\noldCode()\n\n// after\n"
	newContent := "package p\n\n// new comment\nnewCode()\n\n// after\n"
	// oldStart must be the real offset of oldStr (11), and ctx=2 is needed to
	// reach "// after" — it is two lines past the edit (blank line in between).
	got := editDiff(oldContent, newContent, "// old comment\noldCode()", "// new comment\nnewCode()", 11, 2)

	if !strings.Contains(got, "-3\t// old comment") {
		t.Errorf("should show removed line with -:\n%s", got)
	}
	if !strings.Contains(got, "+3\t// new comment") {
		t.Errorf("should show added line with +:\n%s", got)
	}
	if !strings.Contains(got, " 2\t") {
		t.Errorf("should show context before with plain prefix:\n%s", got)
	}
	// Context after must be rendered as context, never as a removal.
	if !strings.Contains(got, " 6\t// after") {
		t.Errorf("should show context after with a plain prefix:\n%s", got)
	}
	if strings.Contains(got, "-6\t// after") {
		t.Errorf("context after must not be reported as removed:\n%s", got)
	}
}

func TestEditFileFoldsSmartQuotes(t *testing.T) {
	// File uses curly quotes; the model reproduces the line with straight ASCII
	// quotes (the transcript failure). The tolerant tier should still match.
	root, rel := writeTemp(t, "f.go", "// 表示一个“工作流产品”，定义模板\npackage x\n")
	content, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  `// 表示一个"工作流产品"，定义模板`,
		"new":  "// 工作流定义",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "verify the result") {
		t.Errorf("expected a tolerant-match note, got: %s", content)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if string(got) != "// 工作流定义\npackage x\n" {
		t.Errorf("replacement not applied at the real byte span, got: %q", got)
	}
}

func TestEditFileStripsLineNumberPrefixes(t *testing.T) {
	// The model copies 'old' straight out of read_file, prefixes and all.
	root, rel := writeTemp(t, "f.go", "package x\n\nfunc A() {}\n")
	if _, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  "3\tfunc A() {}",
		"new":  "func B() {}",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if !strings.Contains(string(got), "func B()") || strings.Contains(string(got), "func A()") {
		t.Errorf("prefixed replacement not applied: %s", got)
	}
}

func TestEditFileNearMissNamesTheDifferingRune(t *testing.T) {
	// A mismatch the tolerant tiers cannot fix: a mistyped identifier. The
	// not-found message should point at the closest line and the differing rune.
	root, rel := writeTemp(t, "f.go", "package x\n\nresult := compute(x)\n")
	content, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  "result := kompute(x)",
		"new":  "result := run(x)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "closest line in the file is line 3") {
		t.Errorf("expected a near-miss pointer to line 3, got: %s", content)
	}
	if !strings.Contains(content, "First difference at column") {
		t.Errorf("expected a first-difference column, got: %s", content)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != "package x\n\nresult := compute(x)\n" {
		t.Errorf("file should be unchanged after a near miss, got: %q", got)
	}
}

func TestEditFilePathEscapeIsError(t *testing.T) {
	root, _ := writeTemp(t, "f.txt", "hi\n")
	if _, err := runEdit(t, root, map[string]any{"path": "../escape.txt", "old": "hi", "new": "bye"}); err == nil {
		t.Fatal("expected an error for a path escaping the workspace")
	}
}
