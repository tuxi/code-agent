package git

import (
	"bytes"
	"code-agent/internal/workspace"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// gogitApplier applies a unified diff in pure Go via go-gitdiff, for iOS. It is
// atomic the way `git apply` is: every file fragment is applied in memory first,
// and the workspace is touched only if all of them succeed — so a patch that does
// not apply changes nothing. Every target path is confined to the workspace.
// Binary patches are not supported (rejected with a clear message).
type gogitApplier struct{}

// pendingWrite is one computed change, held until the whole patch validates.
type pendingWrite struct {
	abs     string // absolute destination path
	data    []byte // new content (ignored when del)
	del     bool   // delete abs instead of writing
	renamed string // absolute old path to remove after a rename (empty otherwise)
}

func (a *gogitApplier) Apply(ctx context.Context, rootAbs, patch string) (string, bool, error) {
	files, _, err := gitdiff.Parse(strings.NewReader(patch))
	if err != nil {
		return "failed to parse patch: " + err.Error(), false, nil
	}
	if len(files) == 0 {
		return "patch contained no file changes", false, nil
	}

	var writes []pendingWrite
	for _, f := range files {
		if f.IsBinary {
			return "binary patches are not supported on this platform", false, nil
		}

		// Destination is NewName for adds/modifies/renames, OldName for deletes.
		// Strip the conventional a/ b/ prefix (go-gitdiff keeps it when the patch
		// lacks a `diff --git` header), matching `git apply -p1`.
		dst := stripPatchPrefix(f.NewName)
		if dst == "" {
			dst = stripPatchPrefix(f.OldName)
		}
		dstAbs, ok := safeJoinWorkspace(rootAbs, dst)
		if !ok {
			return "path escapes workspace: " + dst, false, nil
		}

		if f.IsDelete {
			writes = append(writes, pendingWrite{abs: dstAbs, del: true})
			continue
		}

		// Source content: empty for a new file, else the current file on disk.
		var src []byte
		if !f.IsNew {
			srcName := stripPatchPrefix(f.OldName)
			if srcName == "" {
				srcName = stripPatchPrefix(f.NewName)
			}
			srcAbs, ok := safeJoinWorkspace(rootAbs, srcName)
			if !ok {
				return "path escapes workspace: " + srcName, false, nil
			}
			b, err := os.ReadFile(srcAbs)
			if err != nil {
				return "cannot read " + srcName + ": " + err.Error(), false, nil
			}
			src = b
		}

		var buf bytes.Buffer
		if err := gitdiff.Apply(&buf, bytes.NewReader(src), f); err != nil {
			return "patch did not apply to " + dst + ": " + err.Error(), false, nil
		}

		w := pendingWrite{abs: dstAbs, data: buf.Bytes()}
		if f.IsRename {
			if oldAbs, ok := safeJoinWorkspace(rootAbs, stripPatchPrefix(f.OldName)); ok {
				w.renamed = oldAbs
			}
		}
		writes = append(writes, w)
	}

	// All fragments applied cleanly — commit to disk.
	for _, w := range writes {
		if w.del {
			if err := os.Remove(w.abs); err != nil && !os.IsNotExist(err) {
				return "", false, fmt.Errorf("delete %s: %w", w.abs, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(w.abs), 0o755); err != nil {
			return "", false, fmt.Errorf("mkdir for %s: %w", w.abs, err)
		}
		if err := os.WriteFile(w.abs, w.data, 0o644); err != nil {
			return "", false, fmt.Errorf("write %s: %w", w.abs, err)
		}
		if w.renamed != "" && w.renamed != w.abs {
			if err := os.Remove(w.renamed); err != nil && !os.IsNotExist(err) {
				return "", false, fmt.Errorf("remove renamed-from %s: %w", w.renamed, err)
			}
		}
	}
	return "", true, nil
}

// stripPatchPrefix removes a single conventional git diff prefix ("a/" or "b/")
// from a patch path, the equivalent of `git apply -p1`. go-gitdiff already strips
// it when the patch carries a `diff --git` header; this covers minimal patches that
// only have ---/+++ lines. "/dev/null" and unprefixed names are returned unchanged.
func stripPatchPrefix(name string) string {
	if name == "" || name == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(name, "a/") || strings.HasPrefix(name, "b/") {
		return name[2:]
	}
	return name
}

// safeJoinWorkspace joins a patch-relative path onto the workspace root and reports
// ok=false if it escapes the workspace.
func safeJoinWorkspace(rootAbs, rel string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	abs, err := filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", false
	}
	if !workspace.IsSubPath(rootAbs, abs) {
		return "", false
	}
	return abs, true
}
