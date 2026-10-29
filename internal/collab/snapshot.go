package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"code-agent/internal/processexec"
	"code-agent/internal/sandbox"
	"github.com/pmezard/go-difflib/difflib"
)

const MaxReviewBytes = 16 << 20
const maxFileBytes = 2 << 20
const maxImageBytes = 8 << 20

func ValidateReviewPolicy(p ReviewPolicy) error {
	if len(p.Files) < 1 || len(p.Files) > 64 || len(p.ValidationCommands) < 1 || len(p.ValidationCommands) > 16 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, path := range p.Files {
		if path == "" || !utf8.ValidString(path) || len(path) > 1024 || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || path == "." || strings.HasPrefix(path, "../") || strings.ContainsAny(path, "\x00\r\n\t\\") || seen[path] {
			return ErrInvalid
		}
		for _, part := range strings.Split(path, "/") {
			if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".codeagent") || strings.HasPrefix(strings.ToLower(part), ".env") || sandbox.IsPathProtected(part, sandbox.ProtectedPaths(p.ProtectedPaths)) {
				return ErrForbidden
			}
		}
		seen[path] = true
	}
	for _, command := range p.ValidationCommands {
		if strings.TrimSpace(command) == "" || len(command) > 4096 || strings.ContainsRune(command, 0) {
			return ErrInvalid
		}
	}
	return nil
}

type snapshotBuffer struct {
	bytes.Buffer
	overflow bool
	limit    int
}

func (b *snapshotBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func snapshotGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base := []string{"--no-optional-locks", "--literal-pathspecs", "-c", "core.fsmonitor=false", "-C", root}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1")
	cleanup := processexec.Configure(cmd)
	defer cleanup()
	out := &snapshotBuffer{limit: maxImageBytes}
	stderr := &snapshotBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("snapshot git metadata failed: %w", err)
	}
	if out.overflow || stderr.overflow {
		return nil, ErrInvalid
	}
	return out.Bytes(), nil
}

// CaptureWorkspace never executes a validation command or reads an unlisted
// source file. Changes outside the declared set fail closed, including renames.
func CaptureWorkspace(ctx context.Context, anchor WorkspaceAnchor, policy ReviewPolicy) (WorkspaceImage, error) {
	var empty WorkspaceImage
	if err := ValidateReviewPolicy(policy); err != nil {
		return empty, err
	}
	if err := anchor.Check(anchor.Root); err != nil {
		return empty, err
	}
	one, err := captureWorkspaceOnce(ctx, anchor, policy)
	if err != nil {
		return empty, err
	}
	two, err := captureWorkspaceOnce(ctx, anchor, policy)
	if err != nil {
		return empty, err
	}
	if one.Hash() != two.Hash() {
		return empty, ErrConflict
	}
	if err = anchor.Check(anchor.Root); err != nil {
		return empty, err
	}
	return two, nil
}

func captureWorkspaceOnce(ctx context.Context, anchor WorkspaceAnchor, policy ReviewPolicy) (WorkspaceImage, error) {
	s := WorkspaceImage{Files: []FileImage{}, Index: []FileImage{}}
	head, err := snapshotGit(ctx, anchor.Root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return s, err
	}
	s.Head = strings.TrimSpace(string(head))
	status, err := snapshotGit(ctx, anchor.Root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return s, err
	}
	allowed := map[string]bool{}
	for _, p := range policy.Files {
		allowed[p] = true
	}
	parts := strings.Split(string(status), "\x00")
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 || !allowed[entry[3:]] {
			return s, ErrForbidden
		}
		if strings.ContainsAny(entry[:2], "RC") {
			i++
			if i >= len(parts) || !allowed[parts[i]] {
				return s, ErrForbidden
			}
		}
	}
	s.Status = string(status)
	index, err := snapshotGit(ctx, anchor.Root, "ls-files", "--stage", "-z")
	if err != nil {
		return s, err
	}
	entries := map[string][]string{}
	for _, entry := range strings.Split(string(index), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			if entry != "" {
				return s, ErrInvalid
			}
			continue
		}
		if !allowed[path] {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[2] != "0" || (fields[0] != "100644" && fields[0] != "100755") {
			return s, ErrInvalid
		}
		entries[path] = fields
	}
	paths := append([]string(nil), policy.Files...)
	sort.Strings(paths)
	size := 0
	for _, path := range paths {
		if err = ctx.Err(); err != nil {
			return s, err
		}
		image, err := readReviewFile(anchor.Root, path)
		if err != nil {
			return s, err
		}
		s.Files = append(s.Files, image)
		size += len(image.Data)
		staged := FileImage{Path: path}
		if fields, ok := entries[path]; ok {
			mode, _ := strconv.ParseUint(fields[0], 8, 32)
			data, err := snapshotGit(ctx, anchor.Root, "cat-file", "blob", fields[1])
			if err != nil {
				return s, err
			}
			if len(data) > maxFileBytes {
				return s, ErrInvalid
			}
			staged.Exists = true
			staged.Mode = uint32(mode) & 0777
			staged.Data = data
		}
		size += len(staged.Data)
		if size > maxImageBytes {
			return s, ErrInvalid
		}
		s.Index = append(s.Index, staged)
	}
	encoded, _ := json.Marshal(s)
	if len(encoded) > MaxReviewBytes {
		return s, ErrInvalid
	}
	return s, nil
}

func ImagePatch(before, after []FileImage) (string, error) {
	if len(before) != len(after) {
		return "", ErrInvalid
	}
	var out strings.Builder
	for i, a := range before {
		b := after[i]
		if a.Path != b.Path {
			return "", ErrInvalid
		}
		if a.Exists == b.Exists && a.Mode == b.Mode && bytes.Equal(a.Data, b.Data) {
			continue
		}
		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", a.Path, b.Path)
		gitMode := func(f FileImage) uint32 {
			if f.Mode&0111 != 0 {
				return 0100755
			}
			return 0100644
		}
		if !a.Exists {
			fmt.Fprintf(&out, "new file mode %06o\n", gitMode(b))
		} else if !b.Exists {
			fmt.Fprintf(&out, "deleted file mode %06o\n", gitMode(a))
		} else if gitMode(a) != gitMode(b) {
			fmt.Fprintf(&out, "old mode %06o\nnew mode %06o\n", gitMode(a), gitMode(b))
		}
		if bytes.IndexByte(a.Data, 0) >= 0 || bytes.IndexByte(b.Data, 0) >= 0 || !utf8.Valid(a.Data) || !utf8.Valid(b.Data) {
			out.WriteString("Binary content differs; full before/after bytes are in the manifest.\n")
		} else {
			// Bound difflib's quadratic matching cost. Oversize review surfaces
			// require human handling instead of silently truncating the patch.
			if bytes.Count(a.Data, []byte{'\n'}) > 10000 || bytes.Count(b.Data, []byte{'\n'}) > 10000 {
				return "", ErrInvalid
			}
			from, to := "a/"+a.Path, "b/"+b.Path
			if !a.Exists {
				from = "/dev/null"
			}
			if !b.Exists {
				to = "/dev/null"
			}
			patch, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(string(a.Data)), B: difflib.SplitLines(string(b.Data)), FromFile: from, ToFile: to, Context: 3})
			if err != nil {
				return "", err
			}
			out.WriteString(patch)
		}
		if out.Len() > 4<<20 {
			return "", ErrInvalid
		}
	}
	return out.String(), nil
}

func missingReviewFile(err error) bool { return errors.Is(err, os.ErrNotExist) }
