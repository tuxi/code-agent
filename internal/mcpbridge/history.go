package mcpbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"
)

type historyScope struct {
	configured, root string
	info             os.FileInfo
	err              error
}

func pinHistoryScope(path string) historyScope {
	s := historyScope{configured: path}
	s.root, s.err = filepath.EvalSymlinks(path)
	if s.err == nil {
		s.info, s.err = os.Stat(s.root)
		if s.err == nil && !s.info.IsDir() {
			s.err = os.ErrInvalid
		}
	}
	return s
}

func (s historyScope) check() error {
	current := pinHistoryScope(s.configured)
	if s.err != nil || current.err != nil || s.root != current.root || !os.SameFile(s.info, current.info) {
		return bridgeError("workspace_mismatch", nil)
	}
	return nil
}

func (s *ManualService) historyScope(path string) (historyScope, error) {
	if !filepath.IsAbs(path) {
		return historyScope{}, bridgeError("invalid_tool_arguments", nil)
	}
	for _, scope := range s.readScopes {
		if filepath.Clean(path) == filepath.Clean(scope.configured) {
			return scope, scope.check()
		}
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return historyScope{}, bridgeError("workspace_mismatch", nil)
	}
	for _, scope := range s.readScopes {
		if scope.root == root {
			return scope, scope.check()
		}
	}
	return historyScope{}, bridgeError("read_workspace_not_allowed", nil)
}

type HistoryListInput struct {
	Workspace string `json:"workspace_path"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
}

type HistorySession struct {
	ID            string `json:"session_id"`
	Workspace     string `json:"workspace_path"`
	Name          string `json:"name,omitempty"`
	State         string `json:"turn_status,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	NameTruncated bool   `json:"name_truncated,omitempty"`
}

type HistorySessionPage struct {
	Workspace   string           `json:"workspace_path"`
	Order       string           `json:"order"`
	Sessions    []HistorySession `json:"sessions"`
	NextCursor  string           `json:"next_cursor,omitempty"`
	ScanLimited bool             `json:"scan_limited"`
}

type historyListCursor struct {
	Server, Store, Workspace, Before string
	Archived                         bool
}

// Discovery filters metadata before returning it to MCP. The owner grant is
// still wider than this local allowlist; this is not a server scoped grant.
func (s *ManualService) ListSessions(ctx context.Context, in HistoryListInput) (HistorySessionPage, error) {
	owner, err := s.historyOwner(in.Workspace)
	if err != nil {
		return HistorySessionPage{}, err
	}
	if owner != s {
		return owner.ListSessions(ctx, in)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out HistorySessionPage
	scope, err := s.historyScope(in.Workspace)
	if err != nil {
		return out, err
	}
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Limit < 1 || in.Limit > 50 || len(in.Cursor) > 8192 {
		return out, bridgeError("invalid_tool_arguments", nil)
	}
	c, err := s.probe(ctx, false)
	if err != nil {
		return out, err
	}
	defer c.Close()
	var capabilities struct {
		Capabilities struct {
			Pagination bool `json:"conversation_list_pagination_v1"`
		} `json:"capabilities"`
	}
	if err := c.request(ctx, "GET", "/v1/runtime/capabilities", nil, &capabilities); err != nil {
		return out, err
	}
	if !capabilities.Capabilities.Pagination {
		return out, bridgeError("history_pagination_unsupported", nil)
	}
	cursor := historyListCursor{Server: c.binding.verifiedServerID, Store: c.binding.verifiedStoreID, Workspace: scope.root, Archived: in.Archived}
	if in.Cursor != "" {
		var previous historyListCursor
		raw, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil || json.Unmarshal(raw, &previous) != nil || previous.Server != cursor.Server || previous.Store != cursor.Store || previous.Workspace != cursor.Workspace || previous.Archived != cursor.Archived || previous.Before == "" || len(previous.Before) > 4096 {
			return out, bridgeError("invalid_tool_arguments", nil)
		}
		cursor.Before = previous.Before
	}
	out = HistorySessionPage{Workspace: scope.root, Order: "updated_at_desc,id_desc", Sessions: []HistorySession{}}
	seen := map[string]bool{}
	for scan := 0; scan < 10; scan++ {
		query := url.Values{"limit": {strconv.Itoa(in.Limit - len(out.Sessions))}, "archived": {strconv.FormatBool(in.Archived)}}
		if cursor.Before != "" {
			query.Set("before", cursor.Before)
		}
		var rows []struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace_path"`
			Name      string `json:"name"`
			State     string `json:"turn_status"`
			UpdatedAt string `json:"updated_at"`
		}
		var headers http.Header
		if err := c.requestWithHeaders(ctx, "GET", "/v1/conversations?"+query.Encode(), nil, &rows, &headers); err != nil {
			return HistorySessionPage{}, err
		}
		if len(rows) > in.Limit-len(out.Sessions) {
			return HistorySessionPage{}, bridgeError("evidence_incomplete", nil)
		}
		for _, row := range rows {
			if row.Workspace != scope.root {
				continue
			}
			if !validID(row.ID) || seen[row.ID] {
				return HistorySessionPage{}, bridgeError("evidence_incomplete", nil)
			}
			seen[row.ID] = true
			if len(row.State) > 128 || len(row.UpdatedAt) > 64 {
				return HistorySessionPage{}, bridgeError("evidence_incomplete", nil)
			}
			name := historyPreview(row.Name, 512)
			out.Sessions = append(out.Sessions, HistorySession{ID: row.ID, Workspace: row.Workspace, Name: name.Text, State: row.State, UpdatedAt: row.UpdatedAt, NameTruncated: name.Truncated})
		}
		more := headers.Get("X-Codeagent-Has-More")
		next := headers.Get("X-Codeagent-Next-Cursor")
		if more != "0" && more != "1" || more == "1" && (next == "" || next == cursor.Before || len(next) > 4096) {
			return HistorySessionPage{}, bridgeError("evidence_incomplete", nil)
		}
		cursor.Before = next
		if more == "0" {
			break
		}
		if scan == 9 || len(out.Sessions) == in.Limit {
			raw, _ := json.Marshal(cursor)
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			out.ScanLimited = scan == 9 && len(out.Sessions) < in.Limit
			break
		}
	}
	if err := s.finishHistory(ctx, c, scope); err != nil {
		return HistorySessionPage{}, err
	}
	return out, nil
}

type HistoryReadInput struct {
	Workspace          string `json:"workspace_path"`
	SessionID          string `json:"session_id"`
	Before             int64  `json:"before_cursor,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	IncludeToolDetails bool   `json:"include_tool_details,omitempty"`
}

type HistoryPreview struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func historyPreview(text string, limit int) HistoryPreview {
	preview := HistoryPreview{Text: text}
	if len(text) > limit {
		n := limit
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		preview.Text = text[:n]
		preview.Truncated = true
	}
	return preview
}

type HistoryEvent struct {
	Seq      int64                     `json:"seq"`
	TurnID   string                    `json:"turn_id,omitempty"`
	Kind     string                    `json:"kind"`
	CallID   string                    `json:"call_id,omitempty"`
	ToolName string                    `json:"tool_name,omitempty"`
	Fields   map[string]HistoryPreview `json:"fields,omitempty"`
}
type HistoryPage struct {
	Workspace    string         `json:"workspace_path"`
	SessionID    string         `json:"session_id"`
	Events       []HistoryEvent `json:"events"`
	BeforeCursor int64          `json:"before_cursor"`
	HasMore      bool           `json:"has_more"`
	Truncated    bool           `json:"truncated"`
	TurnFragment bool           `json:"turn_fragment"`
	Note         string         `json:"note"`
}

func (s *ManualService) checkHistorySession(ctx context.Context, c *Client, scope historyScope, id string) error {
	var detail struct {
		ID          string `json:"id"`
		Workspace   string `json:"workspace_path"`
		NeedsRebind bool   `json:"needs_rebind"`
	}
	if err := c.request(ctx, "GET", "/v1/conversations/"+id, nil, &detail); err != nil {
		return err
	}
	if detail.ID != id || detail.Workspace != scope.root || detail.NeedsRebind {
		return bridgeError("session_mismatch", nil)
	}
	return scope.check()
}

// ReadHistory has no Client Session binding, baseline, stream, or control path.
// An exact Session is revalidated before and after each bounded event page.
func (s *ManualService) ReadHistory(ctx context.Context, in HistoryReadInput) (HistoryPage, error) {
	owner, err := s.historyOwner(in.Workspace)
	if err != nil {
		return HistoryPage{}, err
	}
	if owner != s {
		return owner.ReadHistory(ctx, in)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out HistoryPage
	scope, err := s.historyScope(in.Workspace)
	if err != nil {
		return out, err
	}
	if !validID(in.SessionID) || in.Before < 0 {
		return out, bridgeError("invalid_tool_arguments", nil)
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.Limit < 1 || in.Limit > 50 {
		return out, bridgeError("invalid_tool_arguments", nil)
	}
	c, err := s.probe(ctx, false)
	if err != nil {
		return out, err
	}
	defer c.Close()
	if err := s.checkHistorySession(ctx, c, scope, in.SessionID); err != nil {
		return out, err
	}
	query := url.Values{}
	if in.Before == 0 {
		query.Set("tail", strconv.Itoa(in.Limit))
	} else {
		query.Set("before", strconv.FormatInt(in.Before, 10))
		query.Set("limit", strconv.Itoa(in.Limit))
	}
	var rows []map[string]json.RawMessage
	var headers http.Header
	if err := c.requestWithHeaders(ctx, "GET", "/v1/conversations/"+in.SessionID+"/events?"+query.Encode(), nil, &rows, &headers); err != nil {
		return out, err
	}
	out = HistoryPage{Workspace: scope.root, SessionID: in.SessionID, Events: []HistoryEvent{}, Note: "Read-only local event page, not Handoff or acceptance evidence. Fork-source prefixes are not expanded. Tool details and long fields are previews with explicit truncation."}
	for key, dest := range map[string]*bool{"X-Codeagent-Has-More-Older": &out.HasMore, "X-Codeagent-Replay-Truncated": &out.Truncated, "X-Codeagent-Turn-Fragment": &out.TurnFragment} {
		value := headers.Get(key)
		if value != "0" && value != "1" && value != "false" && value != "true" && value != "" {
			return HistoryPage{}, bridgeError("evidence_incomplete", nil)
		}
		*dest = value == "1" || value == "true"
	}
	if headers.Get("X-Codeagent-Has-More-Older") == "" {
		return HistoryPage{}, bridgeError("history_pagination_unsupported", nil)
	}
	var last int64
	for _, row := range rows {
		var event HistoryEvent
		var sessionID string
		if json.Unmarshal(row["seq"], &event.Seq) != nil || event.Seq <= last || in.Before > 0 && event.Seq >= in.Before || json.Unmarshal(row["session_id"], &sessionID) != nil || sessionID != in.SessionID || json.Unmarshal(row["kind"], &event.Kind) != nil {
			return HistoryPage{}, bridgeError("evidence_incomplete", nil)
		}
		last = event.Seq
		if !validID(event.Kind) {
			return HistoryPage{}, bridgeError("evidence_incomplete", nil)
		}
		for key, dest := range map[string]*string{"turn_id": &event.TurnID, "call_id": &event.CallID, "tool_name": &event.ToolName} {
			if raw, ok := row[key]; ok && json.Unmarshal(raw, dest) != nil {
				return HistoryPage{}, bridgeError("evidence_incomplete", nil)
			}
		}
		if len(event.TurnID) > 128 || len(event.CallID) > 128 || len(event.ToolName) > 128 {
			return HistoryPage{}, bridgeError("evidence_incomplete", nil)
		}
		event.Fields = map[string]HistoryPreview{}
		keys := []string{"text", "err", "error", "reason", "cancelled_reason"}
		if in.IncludeToolDetails {
			keys = append(keys, "tool_args", "output", "observation", "chunk")
		}
		if event.Kind == "thinking" || event.Kind == "reasoning_delta" || event.Kind == "token_delta" {
			keys = nil
		}
		for _, key := range keys {
			if raw, ok := row[key]; ok && string(raw) != "null" {
				text := string(raw)
				var decoded string
				if json.Unmarshal(raw, &decoded) == nil {
					text = decoded
				}
				if text == "" {
					continue
				}
				preview := historyPreview(text, 2048)
				out.Truncated = out.Truncated || preview.Truncated
				event.Fields[key] = preview
			}
		}
		out.Events = append(out.Events, event)
	}
	// Server pages are turn-aligned and may exceed the requested row count.
	// Return a bounded suffix; its oldest sequence is the next exclusive cursor.
	for len(out.Events) > 0 {
		encoded, _ := json.Marshal(out.Events)
		if len(out.Events) <= in.Limit && len(encoded) <= 48<<10 {
			break
		}
		out.Events = out.Events[1:]
		out.HasMore = true
		out.TurnFragment = true
	}
	if len(out.Events) > 0 {
		out.BeforeCursor = out.Events[0].Seq
	} else if out.HasMore {
		return HistoryPage{}, bridgeError("evidence_incomplete", nil)
	}
	if err := s.checkHistorySession(ctx, c, scope, in.SessionID); err != nil {
		return HistoryPage{}, err
	}
	if err := s.finishHistory(ctx, c, scope); err != nil {
		return HistoryPage{}, err
	}
	return out, nil
}

func (s *ManualService) finishHistory(ctx context.Context, c *Client, scope historyScope) error {
	if err := scope.check(); err != nil {
		return err
	}
	if err := c.verifyRuntimeIdentity(ctx); err != nil {
		return err
	}
	if err := c.checkBinding(); err != nil {
		return err
	}
	return ctx.Err()
}
