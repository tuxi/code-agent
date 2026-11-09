package fixtureserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/preview"
	"code-agent/internal/preview/fixturetest"
	"code-agent/internal/session"
)

const defaultEvaluationTime = "2026-11-09T00:00:00Z"

type fixtureData struct {
	root           string
	manifest       fixturetest.Manifest
	evaluationTime time.Time
	artifacts      map[string]preview.Artifact // scenario_id -> canonical artifact file
	artifactFiles  map[string]string           // scenario_id -> raw artifact file
	eventFiles     map[string][]string         // scenario_id -> event fixture files
	eventBytes     map[string][]byte
	allArtifacts   map[string]preview.Artifact // session/artifact/revision/hash -> artifact
	baseSession    string
}

type Catalog struct {
	FixtureVersion string     `json:"fixture_version"`
	Contract       string     `json:"contract"`
	EvaluationTime string     `json:"evaluation_time"`
	Capabilities   []string   `json:"capabilities"`
	Scenarios      []Scenario `json:"scenarios"`
}

type Scenario struct {
	ID            string                     `json:"scenario_id"`
	Files         []string                   `json:"files"`
	ArtifactURL   string                     `json:"artifact_url,omitempty"`
	ViewerURL     string                     `json:"viewer_url,omitempty"`
	Assets        []CatalogAsset             `json:"assets,omitempty"`
	EventsURL     string                     `json:"events_url,omitempty"`
	ReplayURL     string                     `json:"replay_url,omitempty"`
	ReplaySession string                     `json:"replay_session_id,omitempty"`
	Expected      map[string]json.RawMessage `json:"expected"`
}

type CatalogAsset struct {
	AssetID   string `json:"asset_id"`
	Role      string `json:"role"`
	URL       string `json:"content_url"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

func loadFixtureData(root string) (fixtureData, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fixtureData{}, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(absRoot, "manifest.json"))
	if err != nil {
		return fixtureData{}, fmt.Errorf("read fixture manifest: %w", err)
	}
	var manifest fixturetest.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fixtureData{}, fmt.Errorf("decode fixture manifest: %w", err)
	}
	if manifest.FixtureVersion != preview.FixtureVersion || manifest.Contract != preview.ContractID {
		return fixtureData{}, fmt.Errorf("unexpected fixture header %s/%s", manifest.FixtureVersion, manifest.Contract)
	}
	data := fixtureData{
		root:          absRoot,
		manifest:      manifest,
		artifacts:     make(map[string]preview.Artifact),
		artifactFiles: make(map[string]string),
		eventFiles:    make(map[string][]string),
		eventBytes:    make(map[string][]byte),
		allArtifacts:  make(map[string]preview.Artifact),
	}
	data.evaluationTime, err = manifestEvaluationTime(manifest)
	if err != nil {
		return fixtureData{}, err
	}
	for _, tc := range manifest.Cases {
		for _, name := range tc.Files {
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			path := filepath.Join(absRoot, filepath.FromSlash(name))
			artifact, ok, err := readTopLevelArtifact(path)
			if err != nil {
				return fixtureData{}, fmt.Errorf("%s: %w", name, err)
			}
			if ok {
				data.artifacts[tc.ScenarioID] = artifact
				data.artifactFiles[tc.ScenarioID] = name
				rememberArtifact(data.allArtifacts, artifact)
				if data.baseSession == "" {
					data.baseSession = artifact.SessionID
				}
			}
			if strings.HasPrefix(filepath.Base(name), "event-") {
				eventBytes, err := os.ReadFile(path)
				if err != nil {
					return fixtureData{}, err
				}
				event, err := preview.DecodeEvent(eventBytes)
				if err != nil {
					return fixtureData{}, fmt.Errorf("%s: %w", name, err)
				}
				if event.Artifact == nil {
					return fixtureData{}, fmt.Errorf("%s: preview event has no artifact", name)
				}
				data.eventFiles[tc.ScenarioID] = append(data.eventFiles[tc.ScenarioID], name)
			}
		}
		if len(data.eventFiles[tc.ScenarioID]) > 0 {
			raw, err := composeEventFixtures(absRoot, tc.Files)
			if err != nil {
				return fixtureData{}, fmt.Errorf("%s: %w", tc.ScenarioID, err)
			}
			data.eventBytes[tc.ScenarioID] = raw
		}
	}
	// Replay keeps the post-fold artifact available even though those scenarios
	// only list event files in the manifest.
	data.artifactFiles["revision-replacement"] = "failed-with-fallback.json"
	data.artifacts["revision-replacement"] = data.artifacts["failed-with-fallback"]
	data.artifactFiles["duplicate-revision"] = "ready-interactive.json"
	data.artifacts["duplicate-revision"] = data.artifacts["ready-interactive"]
	data.artifactFiles["revision-conflict"] = "ready-interactive.json"
	data.artifacts["revision-conflict"] = data.artifacts["ready-interactive"]
	if data.baseSession == "" {
		return fixtureData{}, fmt.Errorf("fixture manifest contains no preview artifact session")
	}
	return data, nil
}

func manifestEvaluationTime(manifest fixturetest.Manifest) (time.Time, error) {
	value := defaultEvaluationTime
	for _, tc := range manifest.Cases {
		if tc.EvaluationAt != "" {
			value = tc.EvaluationAt
			break
		}
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse fixture evaluation time: %w", err)
	}
	return at.UTC(), nil
}

func readTopLevelArtifact(path string) (preview.Artifact, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return preview.Artifact{}, false, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return preview.Artifact{}, false, nil
	}
	var schema string
	if err := json.Unmarshal(obj["schema_version"], &schema); err != nil || schema != preview.SchemaVersion {
		return preview.Artifact{}, false, nil
	}
	var artifact preview.Artifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return preview.Artifact{}, false, err
	}
	return artifact, true, nil
}

func rememberArtifact(out map[string]preview.Artifact, artifact preview.Artifact) {
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", artifact.SessionID, artifact.ArtifactID, artifact.Revision, artifact.RevisionSHA256)
	out[key] = artifact
}

func composeEventFixtures(root string, files []string) ([]byte, error) {
	for _, name := range files {
		if name != "replay-frames.json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%s is not valid JSON", name)
		}
		return raw, nil
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	wrote := false
	for _, name := range files {
		if !strings.HasPrefix(filepath.Base(name), "event-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%s is not valid JSON", name)
		}
		if wrote {
			buf.WriteByte(',')
		}
		buf.Write(bytes.TrimSpace(raw))
		wrote = true
	}
	buf.WriteByte(']')
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("composed replay fixtures are not valid JSON")
	}
	return buf.Bytes(), nil
}

func importFixtureArtifacts(ctx context.Context, store *preview.FSStore, data fixtureData) error {
	keys := make([]string, 0, len(data.allArtifacts))
	for key := range data.allArtifacts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		artifact := data.allArtifacts[key]
		var viewer []byte
		if artifact.Viewer != nil {
			raw, err := os.ReadFile(filepath.Join(data.root, "viewer.html"))
			if err != nil {
				return err
			}
			viewer = raw
		}
		assets := make(map[string][]byte, len(artifact.Assets))
		for _, asset := range artifact.Assets {
			raw, err := os.ReadFile(filepath.Join(data.root, "assets", "fallback.png"))
			if err != nil {
				return err
			}
			assets[asset.AssetID] = raw
		}
		namespace := preview.Namespace{SessionID: artifact.SessionID}
		if err := store.ImportArtifact(ctx, namespace, artifact, viewer, assets); err != nil {
			return fmt.Errorf("import %s revision %d: %w", artifact.ArtifactID, artifact.Revision, err)
		}
	}
	return nil
}

func fixtureSessions(data fixtureData) []string {
	set := map[string]bool{data.baseSession: true}
	for _, artifact := range data.allArtifacts {
		set[artifact.SessionID] = true
	}
	for _, id := range eventScenarioSessions(data.baseSession) {
		set[id] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func eventScenarioSessions(baseSession string) map[string]string {
	return map[string]string{
		"revision-replacement": baseSession + "_revision_replacement",
		"duplicate-revision":   baseSession + "_duplicate_revision",
		"revision-conflict":    baseSession + "_revision_conflict",
	}
}

func seedRuntimeEvents(data fixtureData, events *fixtureEventStore) error {
	baseFiles := []string{"event-revision-1.json"}
	records, err := runtimeEventRecords(data.root, data.baseSession, baseFiles)
	if err != nil {
		return err
	}
	events.add(data.baseSession, records)

	for scenario, sessionID := range eventScenarioSessions(data.baseSession) {
		records, err := runtimeEventRecords(data.root, sessionID, data.eventFiles[scenario])
		if err != nil {
			return fmt.Errorf("%s: %w", scenario, err)
		}
		events.add(sessionID, records)
	}
	return nil
}

func runtimeEventRecords(root, sessionID string, files []string) ([]session.EventRecord, error) {
	out := make([]session.EventRecord, 0, len(files))
	for _, name := range files {
		if !strings.HasPrefix(filepath.Base(name), "event-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		event, err := preview.DecodeEvent(raw)
		if err != nil {
			return nil, err
		}
		artifact := event.Artifact.Clone()
		artifact.SessionID = sessionID
		if artifact.Viewer != nil {
			artifact.Viewer.URL = preview.ViewerURL(sessionID, artifact.ArtifactID, artifact.Revision)
		}
		for i := range artifact.Assets {
			artifact.Assets[i].ContentURL = preview.AssetURL(sessionID, artifact.ArtifactID, artifact.Revision, artifact.Assets[i].AssetID)
		}
		hash, err := preview.RevisionSHA256(artifact)
		if err != nil {
			return nil, err
		}
		artifact.RevisionSHA256 = hash
		if err := artifact.Validate(); err != nil {
			return nil, err
		}
		event.SessionID = sessionID
		event.Artifact = &artifact
		payload, err := json.Marshal(agent.Event{
			Kind:      agent.EventPreviewArtifact,
			At:        artifact.UpdatedAt,
			SessionID: sessionID,
			TurnID:    event.TurnID,
			CallID:    event.CallID,
			Seq:       event.Seq,
			Artifact:  &artifact,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, session.EventRecord{
			Seq: event.Seq, SessionID: sessionID, TurnID: event.TurnID,
			Kind: "preview_artifact", At: artifact.UpdatedAt, Payload: payload,
		})
	}
	return out, nil
}

func buildCatalog(data fixtureData, scenarioSessions map[string]string) Catalog {
	catalog := Catalog{
		FixtureVersion: data.manifest.FixtureVersion,
		Contract:       data.manifest.Contract,
		EvaluationTime: data.evaluationTime.Format(time.RFC3339),
		Capabilities:   preview.FullRuntimeFlags().List(),
		Scenarios:      make([]Scenario, 0, len(data.manifest.Cases)),
	}
	for _, tc := range data.manifest.Cases {
		item := Scenario{ID: tc.ScenarioID, Files: append([]string(nil), tc.Files...), Expected: tc.Expected}
		if file := data.artifactFiles[tc.ScenarioID]; file != "" {
			item.ArtifactURL = "/v1/preview-fixtures/artifacts/" + url.PathEscape(tc.ScenarioID)
		}
		if artifact, ok := data.artifacts[tc.ScenarioID]; ok {
			if artifact.Viewer != nil {
				item.ViewerURL = artifact.Viewer.URL
			}
			for _, asset := range artifact.Assets {
				item.Assets = append(item.Assets, CatalogAsset{
					AssetID: asset.AssetID, Role: string(asset.Role), URL: asset.ContentURL,
					MIMEType: asset.MIMEType, SizeBytes: asset.SizeBytes, SHA256: asset.SHA256,
				})
			}
		}
		if _, ok := data.eventBytes[tc.ScenarioID]; ok {
			item.EventsURL = "/v1/preview-fixtures/events/" + url.PathEscape(tc.ScenarioID)
		}
		if sessionID := scenarioSessions[tc.ScenarioID]; sessionID != "" {
			item.ReplaySession = sessionID
			item.ReplayURL = "/v1/conversations/" + url.PathEscape(sessionID) + "/events"
		}
		catalog.Scenarios = append(catalog.Scenarios, item)
	}
	return catalog
}

type fixtureEventStore struct {
	mu      sync.RWMutex
	records map[string][]session.EventRecord
}

func newFixtureEventStore() *fixtureEventStore {
	return &fixtureEventStore{records: make(map[string][]session.EventRecord)}
}

func (s *fixtureEventStore) add(sessionID string, records []session.EventRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[sessionID] = cloneEventRecords(records)
}

func (s *fixtureEventStore) Append(context.Context, session.EventRecord) (int64, error) {
	return 0, fmt.Errorf("preview fixture event store is read-only")
}

func (s *fixtureEventStore) Replay(_ context.Context, sessionID string) ([]session.EventRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneEventRecords(s.records[sessionID]), nil
}

func (s *fixtureEventStore) ReplaySince(_ context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []session.EventRecord
	for _, record := range s.records[sessionID] {
		if record.Seq > sinceSeq {
			out = append(out, cloneEventRecord(record))
		}
	}
	return out, nil
}

func cloneEventRecords(in []session.EventRecord) []session.EventRecord {
	if in == nil {
		return nil
	}
	out := make([]session.EventRecord, len(in))
	for i, record := range in {
		out[i] = cloneEventRecord(record)
	}
	return out
}

func cloneEventRecord(in session.EventRecord) session.EventRecord {
	in.Payload = append(json.RawMessage(nil), in.Payload...)
	return in
}
