package mcpbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The reviewer view exposes one recorded diff at three granularities. The hash
// levels are deliberately separate so a caller can verify each layer:
//
//	diff_sha256        sha256 of the whole diff, all file patches concatenated
//	file_patch_sha256  sha256 of one file's patch segment
//	chunk_sha256       sha256 of the bytes returned by this single response
//
// Concatenating every file patch in order reproduces the diff byte for byte, so
// diff_sha256 is verifiable from the parts. Concatenating every chunk of one
// file reproduces that file's patch, so file_patch_sha256 is verifiable too.
const (
	defaultChunkBytes = 16 << 10
	maxChunkBytes     = 32 << 10
	maxCommandOutput  = 4 << 10
)

// ErrEvidenceMismatch means the caller asked about a different evidence version
// than the one it was given. Pagination must never mix versions.
var ErrEvidenceMismatch = errors.New("evidence_mismatch: expect_diff_sha256 does not match the bound evidence")

// ArtifactRequest selects a view of one Turn's artifacts. The executor profile
// never fills Mode, so its behaviour is unchanged.
type ArtifactRequest struct {
	TurnID          string
	Mode            string // "" (executor) | summary | file | full
	Path            string
	Offset          int
	Limit           int
	IncludeCommands *bool
	ExpectDiffSHA   string
}

type ArtifactFile struct {
	Path            string `json:"path"`
	Status          string `json:"status"`
	Additions       int    `json:"additions"`
	Deletions       int    `json:"deletions"`
	Binary          bool   `json:"binary"`
	PatchBytes      int    `json:"patch_bytes"`
	FilePatchSHA256 string `json:"file_patch_sha256"`
	ContentProvided bool   `json:"content_provided"`
	Note            string `json:"note,omitempty"`
}

// ArtifactView is the reviewer-facing projection. It is nil for the executor
// default, which keeps that response byte-identical to the previous release.
type ArtifactView struct {
	Mode            string         `json:"mode"`
	Files           []ArtifactFile `json:"files,omitempty"`
	TotalAdditions  int            `json:"total_additions"`
	TotalDeletions  int            `json:"total_deletions"`
	Path            string         `json:"path,omitempty"`
	Status          string         `json:"status,omitempty"`
	Patch           string         `json:"patch,omitempty"`
	Offset          int            `json:"offset"`
	NextOffset      int            `json:"next_offset"`
	ChunkBytes      int            `json:"chunk_bytes,omitempty"`
	ChunkSHA256     string         `json:"chunk_sha256,omitempty"`
	PatchBytes      int            `json:"patch_bytes,omitempty"`
	FilePatchSHA256 string         `json:"file_patch_sha256,omitempty"`
	Binary          bool           `json:"binary,omitempty"`
	ContentProvided bool           `json:"content_provided"`
	Complete        bool           `json:"complete"`
	EvidenceBundle  string         `json:"evidence_bundle,omitempty"`
	BundleSHA256    string         `json:"bundle_sha256,omitempty"`
	Note            string         `json:"note,omitempty"`
}

type diffFile struct {
	path      string
	status    string
	additions int
	deletions int
	binary    bool
	raw       string
	sha       string
}

// splitDiff slices a recorded diff into per-file segments without altering a
// byte: the concatenation of every segment equals the input.
func splitDiff(diff string) ([]diffFile, error) {
	if diff == "" {
		return nil, nil
	}
	const marker = "diff --git "
	var starts []int
	for i := 0; i < len(diff); {
		j := strings.Index(diff[i:], marker)
		if j < 0 {
			break
		}
		pos := i + j
		if pos == 0 || diff[pos-1] == '\n' {
			starts = append(starts, pos)
			i = pos + len(marker)
			continue
		}
		i = pos + len(marker)
	}
	if len(starts) == 0 || starts[0] != 0 {
		return nil, errors.New("diff does not start with a file segment; refusing to split")
	}
	files := make([]diffFile, 0, len(starts))
	for k, start := range starts {
		end := len(diff)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		raw := diff[start:end]
		sum := sha256.Sum256([]byte(raw))
		files = append(files, parseDiffFile(raw, hex.EncodeToString(sum[:])))
	}
	return files, nil
}

func parseDiffFile(raw, sha string) diffFile {
	f := diffFile{raw: raw, sha: sha, status: "modified"}
	var oldPath, newPath string
	var added, deleted, renamed bool
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// Binary segments carry no ---/+++ lines, so the header is the only
			// place their path appears.
			rest := strings.TrimPrefix(line, "diff --git ")
			if i := strings.LastIndex(rest, " b/"); i > 0 {
				oldPath, newPath = diffPath(rest[:i]), diffPath(rest[i+1:])
			}
		case strings.HasPrefix(line, "new file mode"):
			added = true
		case strings.HasPrefix(line, "deleted file mode"):
			deleted = true
		case strings.HasPrefix(line, "rename from "):
			renamed = true
		case strings.HasPrefix(line, "--- "):
			oldPath = diffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			newPath = diffPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
			f.binary = true
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			f.additions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			f.deletions++
		}
	}
	switch {
	case oldPath == "/dev/null" || added:
		f.status = "added"
	case newPath == "/dev/null" || deleted:
		f.status = "deleted"
	case renamed:
		f.status = "renamed"
	}
	switch {
	case newPath != "" && newPath != "/dev/null":
		f.path = newPath
	case oldPath != "" && oldPath != "/dev/null":
		f.path = oldPath
	}
	return f
}

// diffPath strips the a/ b/ prefix git adds and undoes the quoting git applies
// to paths with unusual characters.
func diffPath(s string) string {
	s = strings.TrimSuffix(s, "\r")
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") && len(s) >= 2 {
		if unquoted, err := unquoteGitPath(s); err == nil {
			s = unquoted
		}
	}
	if len(s) > 2 && (s[0] == 'a' || s[0] == 'b') && s[1] == '/' {
		s = s[2:]
	}
	return s
}

// unquoteGitPath handles the C-style quoting git uses for paths containing
// spaces, quotes, or non-ASCII bytes.
func unquoteGitPath(s string) (string, error) {
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", errors.New("truncated escape")
		}
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '0', '1', '2', '3':
			if i+2 >= len(body) {
				return "", errors.New("truncated octal escape")
			}
			v := (int(body[i]-'0') << 6) | (int(body[i+1]-'0') << 3) | int(body[i+2]-'0')
			b.WriteByte(byte(v))
			i += 2
		default:
			return "", errors.New("unsupported escape")
		}
	}
	return b.String(), nil
}

// projectArtifacts applies a reviewer view to an already collected Artifacts
// value. It never mutates the evidence itself; the bundle on disk is untouched.
func projectArtifacts(a Artifacts, req ArtifactRequest, bundlePath, bundleSHA string) (Artifacts, error) {
	if req.ExpectDiffSHA != "" && req.ExpectDiffSHA != a.DiffSHA256 {
		return Artifacts{}, fmt.Errorf("%w (want %s, have %s)", ErrEvidenceMismatch, req.ExpectDiffSHA, a.DiffSHA256)
	}
	mode := req.Mode
	if mode == "" {
		mode = "summary"
	}
	include := false
	if req.IncludeCommands != nil {
		include = *req.IncludeCommands
	}

	files, err := splitDiff(a.Diff)
	if err != nil {
		return Artifacts{}, err
	}
	view := &ArtifactView{Mode: mode, EvidenceBundle: bundlePath, BundleSHA256: bundleSHA}
	view.Files = make([]ArtifactFile, 0, len(files))
	for _, f := range files {
		view.TotalAdditions += f.additions
		view.TotalDeletions += f.deletions
		entry := ArtifactFile{Path: f.path, Status: f.status, Additions: f.additions, Deletions: f.deletions,
			Binary: f.binary, PatchBytes: len(f.raw), FilePatchSHA256: f.sha, ContentProvided: !f.binary}
		if f.binary {
			entry.Note = "binary file: content not provided; this is metadata only and is NOT a visual review"
		}
		view.Files = append(view.Files, entry)
	}
	view.Note = "patch bytes are UTF-8; concatenate all file patches in order to reproduce diff_sha256"

	switch mode {
	case "summary":
		a.Diff = ""
	case "full":
		// keep the whole diff
	case "file":
		var target *diffFile
		for i := range files {
			if files[i].path == req.Path {
				target = &files[i]
				break
			}
		}
		if target == nil {
			return Artifacts{}, fmt.Errorf("path %q is not part of this evidence", req.Path)
		}
		view.Path, view.Status = target.path, target.status
		view.PatchBytes, view.FilePatchSHA256 = len(target.raw), target.sha
		view.Binary, view.ContentProvided = target.binary, !target.binary
		if target.binary {
			view.Complete = true
			view.Note = "binary file: content not provided; metadata only, NOT a visual review"
			a.Diff = ""
			break
		}
		chunk, next, complete, err := sliceUTF8(target.raw, req.Offset, req.Limit)
		if err != nil {
			return Artifacts{}, err
		}
		sum := sha256.Sum256([]byte(chunk))
		view.Patch, view.Offset, view.NextOffset = chunk, req.Offset, next
		view.ChunkBytes, view.ChunkSHA256 = len(chunk), hex.EncodeToString(sum[:])
		view.Complete = complete
		a.Diff = ""
	default:
		return Artifacts{}, fmt.Errorf("unknown diff mode %q (use summary, file or full)", mode)
	}

	if !include {
		a.CommandResults = nil
		a.UnparsedResults = nil
	} else {
		for i := range a.CommandResults {
			c := &a.CommandResults[i]
			if len(c.Stdout) > maxCommandOutput {
				c.Stdout = c.Stdout[:maxCommandOutput]
				c.OutputTruncated = true
			}
			if len(c.Stderr) > maxCommandOutput {
				c.Stderr = c.Stderr[:maxCommandOutput]
				c.OutputTruncated = true
			}
		}
	}
	// Model invocation metadata is execution telemetry, not review evidence, and
	// it dominated the summary payload (7.1 KB of 10.5 KB on the real case). The
	// reviewer view omits it; the executor surface still returns it.
	a.ModelCalls = nil
	a.View = view
	return a, nil
}

// sliceUTF8 returns a byte slice of s that never splits a rune. Offsets are
// UTF-8 byte offsets, which is what the caller sees as patch_bytes.
func sliceUTF8(s string, offset, limit int) (string, int, bool, error) {
	if offset < 0 {
		return "", 0, false, errors.New("offset must not be negative")
	}
	if offset > len(s) {
		return "", 0, false, fmt.Errorf("offset %d is past the %d-byte patch", offset, len(s))
	}
	if offset == len(s) {
		return "", 0, true, nil
	}
	if limit <= 0 {
		limit = defaultChunkBytes
	}
	if limit > maxChunkBytes {
		limit = maxChunkBytes
	}
	end := offset + limit
	if end > len(s) {
		end = len(s)
	}
	// Do not cut inside a multi-byte rune: back off to the rune start.
	for end > offset && end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	if end == offset {
		// A single rune is larger than the requested limit; emit it whole so
		// pagination always makes progress.
		_, size := utf8.DecodeRuneInString(s[offset:])
		end = offset + size
	}
	chunk := s[offset:end]
	if !utf8.ValidString(chunk) {
		return "", 0, false, errors.New("patch slice is not valid UTF-8; refusing to return corrupt text")
	}
	if end >= len(s) {
		return chunk, 0, true, nil
	}
	return chunk, end, false, nil
}
