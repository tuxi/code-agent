package agent

import (
	"encoding/json"
	"sort"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type ContextSourceInfo struct {
	Scope           string `json:"scope,omitempty"`
	Version         string `json:"version,omitempty"`
	ID              string `json:"source_id"`
	Kind            string `json:"kind"`
	Label           string `json:"label"`
	DisplayPath     string `json:"display_path,omitempty"`
	SHA256          string `json:"sha256"`
	EstimatedTokens int    `json:"estimated_tokens"`
}
type ContextToolInfo struct {
	Name            string `json:"name"`
	Origin          string `json:"origin"`
	ConnectorID     string `json:"connector_id,omitempty"`
	Executor        string `json:"executor"`
	SourceID        string `json:"source_id"`
	EstimatedTokens int    `json:"estimated_tokens"`
}
type RequestContextSnapshot struct {
	Model                 string              `json:"model"`
	ContextWindow         int                 `json:"context_window"`
	SnapshotID            string              `json:"snapshot_id"`
	HistoryRevision       string              `json:"history_revision"`
	EstimationMethod      string              `json:"estimation_method"`
	SourceCoverage        string              `json:"source_coverage"`
	Categories            map[string]int      `json:"categories"`
	TotalEstimatedTokens  int                 `json:"total_estimated_tokens"`
	MultimodalUnestimated bool                `json:"multimodal_unestimated"`
	Sources               []ContextSourceInfo `json:"sources"`
	Tools                 []ContextToolInfo   `json:"tools"`
}

func estimateContextText(text string) int { return (len(text) + 2) / 3 }
func sourceInfo(source session.ContextSource) ContextSourceInfo {
	return ContextSourceInfo{Scope: source.Scope, Version: source.Version, ID: source.ID, Kind: source.Kind, Label: source.Label, DisplayPath: source.DisplayPath, SHA256: source.SHA256, EstimatedTokens: estimateContextText(source.Content)}
}

func (r *Runner) captureRequestContext(req model.Request) *RequestContextSnapshot {
	out := &RequestContextSnapshot{Model: req.Model, SnapshotID: r.emitInvocationID, EstimationMethod: "utf8_bytes_div_3_plus_message_overhead_v1", SourceCoverage: "legacy_unattributed", Categories: map[string]int{}, Sources: []ContextSourceInfo{}, Tools: []ContextToolInfo{}}
	sess := r.contextSession
	if sess != nil {
		out.ContextWindow = sess.ContextWindow
		out.HistoryRevision = session.HistoryRevision(sess.Messages)
		if recorded, _ := sess.Metadata["context_source_ledger_v1"].(bool); recorded {
			out.SourceCoverage = "recorded"
		}
		if evicted, _ := sess.Metadata["context_source_catalog_evicted_v1"].(bool); evicted {
			out.SourceCoverage = "catalog_evicted"
		}
	}
	catalog := session.ContextCatalog(sess)
	skillCalls := session.SkillContextCalls(sess)
	seen := map[string]bool{}
	addSource := func(source session.ContextSource) {
		if !seen[source.ID] {
			out.Sources = append(out.Sources, sourceInfo(source))
			seen[source.ID] = true
		}
	}
	for _, message := range req.Messages {
		out.Categories["messages"] += 8
		content := message.Content
		if message.Role == model.RoleSystem {
			matched, remainder := session.MatchContextSources(content, catalog, map[string]bool{"memory_files": true, "skills_index": true})
			for _, source := range matched {
				category := source.Kind
				if category == "skills_index" {
					category = "skills"
				}
				out.Categories[category] += estimateContextText(source.Content)
				addSource(source)
			}
			out.Categories["system_prompt"] += estimateContextText(remainder)
			if sess != nil && remainder != "" {
				addSource(session.RegisterContextSource(sess, "system_prompt", "System instructions and runtime additions", "", remainder))
			}
		} else if source, ok := catalog[skillCalls[message.ToolCallID]]; ok && message.Role == model.RoleTool && source.Content == content {
			out.Categories["skills"] += estimateContextText(content)
			addSource(source)
		} else {
			out.Categories["messages"] += estimateContextText(content)
		}
		out.Categories["messages"] += estimateContextText(message.ReasoningContent)
		for _, call := range message.ToolCalls {
			out.Categories["messages"] += estimateContextText(call.Function.Name + call.Function.Arguments)
		}
		for _, part := range message.ContentParts {
			out.Categories["messages"] += estimateContextText(part.Text)
			if part.ImageURL != nil {
				out.MultimodalUnestimated = true
			}
		}
	}
	for _, definition := range req.Tools {
		body, _ := json.Marshal(definition.Function)
		info := ContextToolInfo{Name: definition.Function.Name, Origin: "unknown", Executor: "unknown", EstimatedTokens: estimateContextText(string(body))}
		category := "other_tools"
		registry := r.contextTools
		if registry == nil {
			registry = r.Tools
		}
		if registry != nil {
			if tool, ok := registry.Get(info.Name); ok {
				info.Origin = "system"
				category = "system_tools"
				if binding, ok := registry.Binding(info.Name); ok {
					info.Executor = string(binding.Executor)
				}
				if provider, ok := tool.(tools.OriginProvider); ok {
					origin := provider.ToolOrigin()
					info.Origin = origin.Kind
					info.ConnectorID = origin.ConnectorID
					if origin.Kind == "mcp" {
						category = "mcp_tools"
					}
				}
			}
		}
		out.Categories[category] += info.EstimatedTokens
		if sess != nil {
			source := session.RegisterContextSource(sess, category, info.Name, "", string(body))
			info.SourceID = source.ID
		}
		out.Tools = append(out.Tools, info)
	}
	if sess != nil {
		if evicted, _ := sess.Metadata["context_source_catalog_evicted_v1"].(bool); evicted {
			out.SourceCoverage = "catalog_evicted"
		}
	}
	for _, count := range out.Categories {
		out.TotalEstimatedTokens += count
	}
	sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].ID < out.Sources[j].ID })
	return out
}
