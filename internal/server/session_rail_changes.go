package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"code-agent/internal/conversation"
	"code-agent/internal/gitworkspace"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

const railDiffLimit = 8 * 1024 * 1024

type railChangedFile struct {
	PreviousPath string `json:"previous_path,omitempty"`
	Submodule    bool   `json:"submodule"`
	ID           string `json:"file_id"`
	Path         string `json:"path"`
	Status       string `json:"status"`
	Added        *int64 `json:"added_lines"`
	Removed      *int64 `json:"removed_lines"`
	Binary       bool   `json:"binary"`
	Conflict     bool   `json:"conflict"`
	PatchState   string `json:"patch_state"`
}
type railChanges struct {
	ObservedAt      string                `json:"observed_at"`
	SnapshotID      string                `json:"snapshot_id"`
	Baseline        string                `json:"baseline"`
	Head            gitworkspace.Checkout `json:"head"`
	IsGitRepository bool                  `json:"is_git_repository"`
	FileCount       int                   `json:"file_count"`
	Added           int64                 `json:"added_lines"`
	Removed         int64                 `json:"removed_lines"`
	UncountedFiles  int                   `json:"uncounted_files"`
	Files           []railChangedFile     `json:"files"`
	patches         map[string]string
}
type railChangesEntry struct {
	root  string
	scope string
	at    time.Time
	data  railChanges
}
type railChangesStore struct {
	mu      sync.Mutex
	entries map[string]railChangesEntry
}

// Each mux retains a bounded set of immutable patches. A detail request either
// reads that exact snapshot or returns stale; it never substitutes today's diff.
func newRailChangesStore() *railChangesStore {
	return &railChangesStore{entries: map[string]railChangesEntry{}}
}
func (s *railChangesStore) capture(ctx context.Context, scope, root string) (railChanges, error) {
	s.mu.Lock()
	entry, ok := s.entries[scope]
	s.mu.Unlock()
	if ok && entry.root == root && time.Since(entry.at) < 2*time.Second {
		return entry.data, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := readRailChanges(ctx, root)
	if err != nil {
		return data, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= 8 {
		oldest := ""
		var at time.Time
		for k, e := range s.entries {
			if oldest == "" || e.at.Before(at) {
				oldest, at = k, e.at
			}
		}
		delete(s.entries, oldest)
	}
	data.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.entries[scope] = railChangesEntry{root: root, scope: scope, at: time.Now(), data: data}
	return data, nil
}
func (s *railChangesStore) patch(scope, root, snapshotID, fileID string) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[scope]
	if !ok || entry.root != root || entry.data.SnapshotID != snapshotID || time.Since(entry.at) > 5*time.Minute {
		return "", "", false
	}
	for _, f := range entry.data.Files {
		if f.ID == fileID {
			return entry.data.patches[fileID], f.PatchState, true
		}
	}
	return "", "", false
}

type railGitBuffer struct {
	bytes.Buffer
	limit int
}

func (b *railGitBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("git output exceeds snapshot budget")
	}
	return b.Buffer.Write(p)
}
func railGit(ctx context.Context, root string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	out := &railGitBuffer{limit: railDiffLimit}
	stderr := &railGitBuffer{limit: 8192}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	return out.String(), stderr.String(), err
}
func readRailChanges(ctx context.Context, root string) (railChanges, error) {
	out := railChanges{Baseline: "HEAD", Head: gitworkspace.Checkout{Kind: "unknown"}, Files: []railChangedFile{}, patches: map[string]string{}}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(root) {
		return out, errors.New("workspace_unavailable")
	}
	top, stderr, err := railGit(ctx, canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		if strings.Contains(stderr, "not a git repository") {
			out.Head.Kind = "not_git"
			out.SnapshotID = railRevision(struct{ Root, Kind string }{canonical, "not_git"})
			return out, nil
		}
		return out, errors.New("git_unavailable")
	}
	top = strings.TrimSuffix(top, "\n")
	topCanonical, err := filepath.EvalSymlinks(top)
	if err != nil || topCanonical != canonical {
		return out, errors.New("workspace_not_git_root")
	}
	out.IsGitRepository = true
	head, _, headErr := railGit(ctx, canonical, "rev-parse", "--verify", "HEAD")
	branch, _, branchErr := railGit(ctx, canonical, "symbolic-ref", "--quiet", "--short", "HEAD")
	base := strings.TrimSpace(head)
	if headErr != nil {
		if branchErr != nil {
			return out, errors.New("git_head_unavailable")
		}
		out.Head.Kind = "unborn"
		empty, _, e := railGit(ctx, canonical, "hash-object", "-t", "tree", "--stdin")
		if e != nil {
			return out, errors.New("git_baseline_unavailable")
		}
		base = strings.TrimSpace(empty)
	} else {
		out.Head.Commit = base
		if branchErr == nil {
			out.Head.Kind = "branch"
		} else {
			out.Head.Kind = "detached"
		}
	}
	if branchErr == nil {
		name := strings.TrimSuffix(branch, "\n")
		out.Head.Name = &name
	}
	statusArgs := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none", "--no-renames"}
	diffArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames", "--ignore-submodules=none", base, "--"}
	status, _, err := railGit(ctx, canonical, statusArgs...)
	if err != nil {
		return out, errors.New("git_status_unavailable")
	}
	diff, _, err := railGit(ctx, canonical, diffArgs...)
	if err != nil {
		return out, errors.New("git_diff_unavailable")
	}
	// Reject a moving worktree; the cached body and patches are one observation.
	statusAfter, _, e1 := railGit(ctx, canonical, statusArgs...)
	diffAfter, _, e2 := railGit(ctx, canonical, diffArgs...)
	headAfter, _, e3 := railGit(ctx, canonical, "rev-parse", "--verify", "HEAD")
	branchAfter, _, _ := railGit(ctx, canonical, "symbolic-ref", "--quiet", "--short", "HEAD")
	if e1 != nil || e2 != nil || status != statusAfter || diff != diffAfter || head != headAfter || branch != branchAfter || (headErr == nil) != (e3 == nil) {
		return out, errors.New("workspace_snapshot_changed")
	}
	parsed, _, err := gitdiff.Parse(strings.NewReader(diff))
	if err != nil {
		return out, errors.New("git_diff_unavailable")
	}
	statuses := map[string]string{}
	for _, line := range strings.Split(status, "\x00") {
		if len(line) >= 4 {
			statuses[line[3:]] = line[:2]
		}
	}
	for _, f := range parsed {
		path := f.NewName
		if f.IsDelete {
			path = f.OldName
		}
		row := railChangedFile{ID: railRevision(path), Path: path, Status: "modified", Binary: f.IsBinary, PatchState: "ready"}
		if f.IsRename {
			row.Status = "renamed"
			row.PreviousPath = f.OldName
		}
		row.Submodule = f.OldMode == 0160000 || f.NewMode == 0160000
		if f.IsNew {
			row.Status = "added"
		}
		if f.IsDelete {
			row.Status = "deleted"
		}
		st := statuses[path]
		row.Conflict = strings.Contains(st, "U") || st == "AA" || st == "DD"
		if row.Binary || row.Conflict || row.Submodule {
			out.UncountedFiles++
			row.PatchState = "binary"
			if row.Submodule {
				row.PatchState = "submodule"
			}
			if row.Conflict {
				row.PatchState = "conflict"
			}
		} else {
			var added, removed int64
			for _, fragment := range f.TextFragments {
				added += fragment.LinesAdded
				removed += fragment.LinesDeleted
			}
			row.Added = &added
			row.Removed = &removed
			out.Added += added
			out.Removed += removed
		}
		out.Files = append(out.Files, row)
		out.patches[row.ID] = f.String()
	}
	for path, st := range statuses {
		if st == "??" {
			out.Files = append(out.Files, railChangedFile{ID: railRevision(path), Path: path, Status: "untracked", PatchState: "untracked"})
			out.UncountedFiles++
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	out.FileCount = len(out.Files)
	out.SnapshotID = railRevision(struct{ Root, Base, Branch, Status, Diff string }{canonical, base, branch, status, diff})
	return out, nil
}

func registerRailChangesRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, store *railChangesStore) {
	mux.HandleFunc("GET /v1/conversations/{id}/changes", func(w http.ResponseWriter, r *http.Request) {
		sess, err := loadConversationHeader(r.Context(), repo, r.PathValue("id"))
		if err != nil {
			ErrorSimple(w, r, 404, "conversation not found")
			return
		}
		if rebind, e := repo.NeedsRebind(r.Context(), sess.ID); e != nil {
			ErrorSimple(w, r, 503, "workspace unavailable")
			return
		} else if rebind {
			ErrorSimple(w, r, 409, "workspace needs rebind")
			return
		}
		data, err := store.capture(r.Context(), sess.ID, sess.WorkspacePath)
		if err != nil {
			Result(w, r, 503, 50300, "workspace changes unavailable", map[string]string{"code": err.Error()})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, 200, data)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/changes/{snapshot_id}/{file_id}", func(w http.ResponseWriter, r *http.Request) {
		sess, err := loadConversationHeader(r.Context(), repo, r.PathValue("id"))
		if err != nil {
			ErrorSimple(w, r, 404, "conversation not found")
			return
		}
		if rebind, e := repo.NeedsRebind(r.Context(), sess.ID); e != nil {
			ErrorSimple(w, r, 503, "workspace unavailable")
			return
		} else if rebind {
			ErrorSimple(w, r, 409, "workspace needs rebind")
			return
		}
		patch, state, ok := store.patch(sess.ID, sess.WorkspacePath, r.PathValue("snapshot_id"), r.PathValue("file_id"))
		if !ok {
			Result(w, r, 409, 40900, "change snapshot expired", map[string]string{"code": "stale_snapshot"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, 200, map[string]string{"snapshot_id": r.PathValue("snapshot_id"), "file_id": r.PathValue("file_id"), "state": state, "patch": patch})
	})
}
