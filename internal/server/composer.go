package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"code-agent/internal/conversation"
)

// Composer queries are read-only. A draft never creates a runtime session just
// to discover commands or files. Only full-desktop runtimes accept draft paths.
type composerQuery struct {
	SessionID     string `json:"session_id,omitempty"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	Kind          string `json:"kind"`
	Query         string `json:"query"`
	Revision      int64  `json:"revision"`
}
type composerItem struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	Detail       string `json:"detail"`
	InsertText   string `json:"insert_text"`
	Action       string `json:"action"`
	RelativePath string `json:"relative_path,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
}
type composerResult struct {
	Schema    string         `json:"schema"`
	Revision  int64          `json:"revision"`
	Items     []composerItem `json:"items"`
	Truncated bool           `json:"truncated"`
}

func registerComposerRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, opts MuxOptions) {
	mux.HandleFunc("POST /v1/composer/query", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		var req composerQuery
		if err := decodeBoundedJSON(w, r, &req, 16<<10); err != nil || len(req.Query) > 512 || (req.Kind != "command" && req.Kind != "reference") {
			Result(w, r, http.StatusBadRequest, CodeBadRequest, "invalid composer query", nil)
			return
		}
		root, err := composerWorkspace(r.Context(), repo, opts, req)
		if err != nil {
			Result(w, r, http.StatusForbidden, 40300, err.Error(), nil)
			return
		}
		out := composerResult{Schema: "composer-query/v1", Revision: req.Revision, Items: []composerItem{}}
		query := strings.ToLower(strings.TrimSpace(req.Query))
		if req.Kind == "command" {
			out.Items = []composerItem{
				{ID: "client:model", Kind: "command", Label: "model", Detail: "选择模型与推理强度", Action: "model"},
				{ID: "client:permissions", Kind: "command", Label: "permissions", Detail: "查看工作区权限", Action: "permissions"},
				{ID: "client:effort", Kind: "command", Label: "effort", Detail: "调整推理强度", Action: "model"},
				{ID: "client:skills", Kind: "command", Label: "skills", Detail: "查看与管理可用技能", Action: "skills"},
			}
			if opts.SkillPlugins != nil {
				inventory, err := opts.SkillPlugins.SkillInventory(r.Context(), req.SessionID, root)
				if err != nil {
					Result(w, r, http.StatusServiceUnavailable, CodeInternal, "skill inventory unavailable", nil)
					return
				}
				for _, skill := range inventory.Items {
					if skill.Enabled && skill.ShadowedBy == nil && len(skill.ParseErrors) == 0 {
						out.Items = append(out.Items, composerItem{ID: "skill:" + skill.Name, Kind: "skill", Label: skill.Name, Detail: skill.Description, InsertText: "使用技能 " + skill.Name, Action: "insert"})
					}
				}
			}
			filtered := out.Items[:0]
			for _, item := range out.Items {
				if query == "" || strings.Contains(strings.ToLower(item.Label), query) {
					filtered = append(filtered, item)
				}
			}
			out.Items = filtered
		} else {
			if root != "" {
				items, truncated, err := searchComposerFiles(r.Context(), root, query)
				if err != nil {
					Result(w, r, http.StatusBadRequest, CodeBadRequest, "workspace is not readable", nil)
					return
				}
				out.Items = append(out.Items, items...)
				out.Truncated = truncated
			}
			if repo != nil && root != "" {
				sessions, err := repo.List(r.Context())
				if err != nil {
					ErrorSimple(w, r, http.StatusServiceUnavailable, "conversation inventory unavailable")
					return
				}
				recent := []composerItem{}
				for _, s := range sessions {
					if s.ID == req.SessionID || filepath.Clean(s.WorkspacePath) != root || !s.ArchivedAt.IsZero() {
						continue
					}
					label := s.Name
					if label == "" {
						label = s.Title
					}
					if label == "" {
						continue
					}
					if query != "" && !strings.Contains(strings.ToLower(label), query) {
						continue
					}
					recent = append(recent, composerItem{ID: "session:" + s.ID, Kind: "conversation", Label: label, Detail: "最近对话 · " + filepath.Base(root), SessionID: s.ID, InsertText: label, Action: "insert"})
					if len(recent) >= 3 {
						break
					}
				}
				out.Items = append(recent, out.Items...)
			}

		}
		if len(out.Items) > 40 {
			out.Items = out.Items[:40]
			out.Truncated = true
		}
		writeJSON(w, r, http.StatusOK, out)
	})
}

func composerWorkspace(ctx context.Context, repo conversation.ConversationRepository, opts MuxOptions, req composerQuery) (string, error) {
	root := ""
	if req.SessionID != "" {
		if repo == nil {
			return "", errors.New("conversation not found")
		}
		s, err := loadConversationHeader(ctx, repo, req.SessionID)
		if err != nil {
			return "", errors.New("conversation not found")
		}
		needsRebind, err := repo.NeedsRebind(ctx, req.SessionID)
		if err != nil || needsRebind {
			return "", errors.New("workspace must be rebound")
		}
		if req.WorkspacePath != "" {
			return "", errors.New("session scope cannot be overridden")
		}
		root = s.WorkspacePath
	} else if req.WorkspacePath != "" {
		if opts.RuntimeInfo.RuntimeProfile != RuntimeProfileFullDesktop {
			return "", errors.New("draft paths require a full desktop runtime")
		}
		root = req.WorkspacePath
	}
	if root == "" {
		return "", nil
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("workspace must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	stat, err := os.Stat(canonical)
	if err != nil || !stat.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func searchComposerFiles(ctx context.Context, root, query string) ([]composerItem, bool, error) {
	items := []composerItem{}
	visited := 0
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if path == root {
			return nil
		}
		visited++
		if visited > 5000 {
			truncated = true
			return fs.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", ".build", ".codeagent", ".next", "vendor":
				return fs.SkipDir
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if query != "" && !strings.Contains(strings.ToLower(rel), query) {
			return nil
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		digest := sha256.Sum256([]byte(kind + "\x00" + rel))
		encoded, _ := json.Marshal(rel)
		items = append(items, composerItem{ID: kind + ":" + hex.EncodeToString(digest[:12]), Kind: kind, Label: entry.Name(), Detail: rel, RelativePath: rel, InsertText: string(encoded), Action: "insert"})
		if len(items) >= 40 {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	sort.SliceStable(items, func(i, j int) bool { return items[i].RelativePath < items[j].RelativePath })
	return items, truncated, err
}
