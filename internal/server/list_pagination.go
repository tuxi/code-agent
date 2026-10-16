package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

type listPageStorageError struct{ error }

func listActivityMetadata(ctx context.Context, repo conversation.ConversationRepository) ([]session.Meta, error) {
	if metadata, ok := repo.(session.SessionMetadataStore); ok {
		return metadata.ListMetadata(ctx)
	}
	return repo.List(ctx)
}

func supportsListPagination(repo conversation.ConversationRepository) bool {
	_, supported := repo.(session.SessionPageStore)
	if capability, ok := repo.(interface{ SupportsConversationListPagination() bool }); ok {
		supported = supported && capability.SupportsConversationListPagination()
	}
	return supported
}

func loadConversationPage(w http.ResponseWriter, r *http.Request, repo conversation.ConversationRepository, archived bool) ([]session.Meta, error) {
	if !supportsListPagination(repo) {
		return nil, fmt.Errorf("conversation list pagination is unsupported")
	}
	limit := 50
	if raw, present := r.URL.Query()["limit"]; present {
		if len(raw) != 1 {
			return nil, fmt.Errorf("limit must be between 1 and 200")
		}
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > 200 {
			return nil, fmt.Errorf("limit must be between 1 and 200")
		}
		limit = n
	}
	var before *session.SessionListCursor
	if raw, present := r.URL.Query()["before"]; present {
		if len(raw) != 1 || len(raw[0]) > 4096 {
			return nil, fmt.Errorf("invalid before cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(raw[0])
		if err != nil {
			return nil, fmt.Errorf("invalid before cursor")
		}
		before = &session.SessionListCursor{}
		if json.Unmarshal(data, before) != nil || before.ID == "" || before.Archived != archived {
			return nil, fmt.Errorf("invalid before cursor")
		}
	}
	metas, next, err := repo.(session.SessionPageStore).ListSessionPage(r.Context(), archived, before, limit)
	if err != nil {
		return nil, &listPageStorageError{err}
	}
	w.Header().Set("X-Codeagent-Has-More", "0")
	if next != nil {
		data, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		w.Header().Set("X-Codeagent-Next-Cursor", base64.RawURLEncoding.EncodeToString(data))
		w.Header().Set("X-Codeagent-Has-More", "1")
	}
	return metas, nil
}
