package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"code-agent/internal/model"
)

const ContextCatalogKey = "context_source_catalog_v1"
const ContextSkillCallsKey = "context_skill_calls_v1"

// Content is stored once per version in the checkpoint, never duplicated into
// every model_request event. An evicted version is unavailable, not today's file.
type ContextSource struct {
	Scope       string `json:"scope,omitempty"`
	Version     string `json:"version,omitempty"`
	ID          string `json:"source_id"`
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	DisplayPath string `json:"display_path,omitempty"`
	SHA256      string `json:"sha256"`
	Content     string `json:"content"`
	Prefix      string `json:"prefix,omitempty"`
}

func ContentHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}
func ContextCatalog(s *Session) map[string]ContextSource {
	out := map[string]ContextSource{}
	if s == nil {
		return out
	}
	if typed, ok := s.Metadata[ContextCatalogKey].(map[string]ContextSource); ok && typed != nil {
		return typed
	}
	b, err := json.Marshal(s.Metadata[ContextCatalogKey])
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	if out == nil {
		out = map[string]ContextSource{}
	}
	if s.Metadata != nil {
		if _, exists := s.Metadata[ContextCatalogKey]; exists {
			s.Metadata[ContextCatalogKey] = out
		}
	}
	return out
}
func RegisterContextSource(s *Session, kind, label, path, content string, identity ...string) ContextSource {
	source := ContextSource{Kind: kind, Label: label, DisplayPath: path, SHA256: ContentHash(content), Content: content}
	anchor := path
	if len(identity) > 0 {
		anchor = identity[0]
	}
	source.ID = ContentHash(kind + "\x00" + label + "\x00" + anchor + "\x00" + source.SHA256)
	catalog := ContextCatalog(s)
	if _, ok := catalog[source.ID]; ok {
		return source
	}
	// Retain a bounded version catalog. Existing request IDs survive eviction;
	// their detail endpoint reports that the exact version is unavailable.
	size := len(content)
	for _, entry := range catalog {
		size += len(entry.Content) + len(entry.Prefix)
	}
	if len(catalog) >= 2048 || size > 4*1024*1024 {
		catalog = map[string]ContextSource{}
		if s.Metadata == nil {
			s.Metadata = map[string]any{}
		}
		s.Metadata["context_source_catalog_evicted_v1"] = true
	}
	if len(content) <= 4*1024*1024 {
		catalog[source.ID] = source
	}
	if s.Metadata == nil {
		s.Metadata = map[string]any{}
	}
	s.Metadata[ContextCatalogKey] = catalog
	return source
}
func HistoryRevision(messages []model.Message) string {
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for _, message := range messages {
		_ = encoder.Encode(message)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func SkillContextCalls(s *Session) map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	b, _ := json.Marshal(s.Metadata[ContextSkillCallsKey])
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]string{}
	}
	return out
}

// MatchContextSources attributes exact, nonoverlapping content only. Changed
// hooks or pruning invalidate a match rather than claiming old content is live.
func MatchContextSources(content string, catalog map[string]ContextSource, kinds map[string]bool) ([]ContextSource, string) {
	var found []ContextSource
	// Catalog order is normalized by the caller through longest-first matching.
	entries := make([]ContextSource, 0, len(catalog))
	for _, source := range catalog {
		if kinds[source.Kind] && source.Content != "" {
			entries = append(entries, source)
		}
	}
	sortContextSources(entries)
	type span struct{ start, end int }
	used := []span{}
	for _, source := range entries {
		needle := source.Content
		bodyOffset := 0
		if source.Prefix != "" {
			needle = source.Prefix + source.Content + "\n</project_instructions>"
			bodyOffset = len(source.Prefix)
		}
		for offset := 0; offset <= len(content)-len(needle); {
			relative := strings.Index(content[offset:], needle)
			if relative < 0 {
				break
			}
			start := offset + relative + bodyOffset
			end := start + len(source.Content)
			overlap := false
			for _, interval := range used {
				if start < interval.end && end > interval.start {
					overlap = true
					break
				}
			}
			if !overlap {
				found = append(found, source)
				used = append(used, span{start, end})
				break
			}
			offset = start + 1
		}
	}
	sort.Slice(used, func(i, j int) bool { return used[i].start < used[j].start })
	var remaining strings.Builder
	offset := 0
	for _, interval := range used {
		remaining.WriteString(content[offset:interval.start])
		offset = interval.end
	}
	remaining.WriteString(content[offset:])
	content = remaining.String()

	return found, content
}

func sortContextSources(entries []ContextSource) {
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].Content) != len(entries[j].Content) {
			return len(entries[i].Content) > len(entries[j].Content)
		}
		return entries[i].ID < entries[j].ID
	})
}
