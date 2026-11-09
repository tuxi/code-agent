// Package fixturetest executes the frozen preview v1 manifest against the Go
// runtime. It is reusable by backend CI and cross-language contract harnesses.
package fixturetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/preview"
)

type Manifest struct {
	FixtureVersion string         `json:"fixture_version"`
	Contract       string         `json:"contract"`
	Cases          []ManifestCase `json:"cases"`
}

type ManifestCase struct {
	Files        []string                   `json:"files"`
	Expected     map[string]json.RawMessage `json:"expected"`
	ScenarioID   string                     `json:"scenario_id"`
	FileDigests  map[string]string          `json:"file_digests"`
	EvaluationAt string                     `json:"evaluation_time,omitempty"`
}

type Report struct {
	Contract  string
	Scenarios []string
}

func Run(root string) (Report, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return Report{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Report{}, err
	}
	if manifest.FixtureVersion != preview.FixtureVersion || manifest.Contract != preview.ContractID {
		return Report{}, fmt.Errorf("unexpected fixture header %s/%s", manifest.FixtureVersion, manifest.Contract)
	}
	report := Report{Contract: manifest.Contract}
	var failures []error
	for _, tc := range manifest.Cases {
		if err := verifyDigests(root, tc); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", tc.ScenarioID, err))
			continue
		}
		var runErr error
		switch tc.ScenarioID {
		case "ready-interactive", "static-image", "pending", "unavailable", "failed-with-fallback", "failed-without-fallback", "unavailable-with-fallback":
			runErr = runArtifactCase(root, tc)
		case "expiry-derived":
			runErr = runArtifactCase(root, tc)
		case "revision-replacement", "duplicate-revision", "revision-conflict":
			runErr = runRevisionCase(root, tc)
		case "capability-matrix":
			runErr = runCapabilityMatrix(root)
		case "producer-shape":
			runErr = runProducerShape(root, tc)
		case "negative-security":
			runErr = runSecurityCases(root, tc)
		case "viewer-size-boundary":
			runErr = runSizeBoundary(root, tc)
		case "transport-neutral-resolver":
			runErr = runResolver(root, tc)
		default:
			runErr = fmt.Errorf("unhandled scenario %s", tc.ScenarioID)
		}
		if runErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", tc.ScenarioID, runErr))
			continue
		}
		report.Scenarios = append(report.Scenarios, tc.ScenarioID)
	}
	if len(failures) > 0 {
		return report, errors.Join(failures...)
	}
	return report, nil
}

func verifyDigests(root string, tc ManifestCase) error {
	for _, name := range tc.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		want := tc.FileDigests[name]
		if want != "" && !preview.DigestMatches(want, data) {
			return fmt.Errorf("%s digest = %s, want %s", name, preview.DigestBytes(data), want)
		}
	}
	return nil
}

func runArtifactCase(root string, tc ManifestCase) error {
	artifact, err := loadArtifact(filepath.Join(root, tc.Files[0]))
	if err != nil {
		return err
	}
	if got, err := RevisionSHA(artifact); err != nil || !strings.EqualFold(got, artifact.RevisionSHA256) {
		if err != nil {
			return err
		}
		return fmt.Errorf("revision hash = %s, want %s", got, artifact.RevisionSHA256)
	}
	if err := artifact.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if tc.EvaluationAt != "" {
		now, err = time.Parse(time.RFC3339, tc.EvaluationAt)
		if err != nil {
			return err
		}
	}
	state := preview.DerivePresentation(preview.PresentationInput{
		Artifact:     artifact,
		Capabilities: preview.Negotiate(preview.FullRuntimeFlags().List(), preview.FullRuntimeFlags().List(), false),
		Now:          now,
	})
	expected := decodeExpected(tc.Expected)
	if want, _ := expected["presentation"].(string); want != "" && string(state) != want {
		return fmt.Errorf("presentation = %s, want %s", state, want)
	}
	if want, _ := expected["artifact_status"].(string); want != "" && string(artifact.Status) != want {
		return fmt.Errorf("status = %s, want %s", artifact.Status, want)
	}
	if want, _ := expected["kind"].(string); want != "" && string(artifact.Kind) != want {
		return fmt.Errorf("kind = %s, want %s", artifact.Kind, want)
	}
	if want, _ := expected["error_code"].(string); want != "" && (artifact.Error == nil || string(artifact.Error.Code) != want) {
		return fmt.Errorf("error code = %+v, want %s", artifact.Error, want)
	}
	if want, _ := expected["fallback_asset_id"].(string); want != "" && (artifact.Fallback == nil || artifact.Fallback.AssetID != want) {
		return fmt.Errorf("fallback = %+v, want %s", artifact.Fallback, want)
	}
	return nil
}

func runRevisionCase(root string, tc ManifestCase) error {
	var events []preview.Event
	for _, name := range tc.Files {
		if !strings.HasPrefix(name, "event-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return err
		}
		event, err := preview.DecodeEvent(data)
		if err != nil {
			return err
		}
		events = append(events, event)
	}
	result := preview.ApplyReplay(events)
	expected := decodeExpected(tc.Expected)
	if want, _ := expected["error_code"].(string); want != "" {
		if result.Error == nil || string(result.Error.Code) != want {
			return fmt.Errorf("replay error = %+v, want %s", result.Error, want)
		}
	}
	if want, _ := expected["final_artifact_id"].(string); want != "" {
		if len(result.Artifacts) != 1 || result.Artifacts[0].ArtifactID != want {
			return fmt.Errorf("final artifacts = %+v, want %s", result.Artifacts, want)
		}
	}
	if want, _ := expected["preserved_artifact_id"].(string); want != "" {
		if len(result.Artifacts) != 1 || result.Artifacts[0].ArtifactID != want {
			return fmt.Errorf("preserved artifacts = %+v, want %s", result.Artifacts, want)
		}
	}
	if want := int64From(expected["final_revision"]); want > 0 {
		if len(result.Artifacts) != 1 || result.Artifacts[0].Revision != want {
			return fmt.Errorf("final revision = %+v, want %d", result.Artifacts, want)
		}
	}
	if want := int64From(expected["preserved_revision"]); want > 0 {
		if len(result.Artifacts) != 1 || result.Artifacts[0].Revision != want {
			return fmt.Errorf("preserved revision = %+v, want %d", result.Artifacts, want)
		}
	}
	if want, _ := expected["final_status"].(string); want != "" {
		if len(result.Artifacts) != 1 || string(result.Artifacts[0].Status) != want {
			return fmt.Errorf("final status = %+v, want %s", result.Artifacts, want)
		}
	}
	if want := int64From(expected["block_count"]); want > 0 && int64(len(result.Artifacts)) != want {
		return fmt.Errorf("block count = %d, want %d", len(result.Artifacts), want)
	}
	return nil
}

func runCapabilityMatrix(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "capability-matrix.json"))
	if err != nil {
		return err
	}
	var fixture struct {
		Cases []struct {
			Runtime  []string `json:"runtime"`
			Client   []string `json:"client"`
			Expected string   `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err
	}
	for i, tc := range fixture.Cases {
		got := preview.Negotiate(tc.Runtime, tc.Client, false)
		if string(got.LiveMode) != tc.Expected {
			return fmt.Errorf("case %d live mode = %s, want %s", i, got.LiveMode, tc.Expected)
		}
	}
	return nil
}

func runProducerShape(root string, tc ManifestCase) error {
	bindingsData, err := os.ReadFile(filepath.Join(root, "tool-bindings.json"))
	if err != nil {
		return err
	}
	var bindings struct {
		RegisterTools struct {
			Tools []agent.ClientToolDef `json:"tools"`
		} `json:"register_tools"`
	}
	if err := json.Unmarshal(bindingsData, &bindings); err != nil {
		return err
	}
	if err := agent.ValidateClientToolDefs(bindings.RegisterTools.Tools); err != nil {
		return err
	}
	if len(bindings.RegisterTools.Tools) != 2 || bindings.RegisterTools.Tools[0].ModelVisibility != "hidden" || bindings.RegisterTools.Tools[1].Executor != "flex" {
		return fmt.Errorf("tool binding fixture shape is invalid: %+v", bindings.RegisterTools.Tools)
	}
	var producer struct {
		ServerTool struct {
			Previews []preview.Draft `json:"previews"`
		} `json:"server_tool"`
		HiddenClientTool struct {
			Previews []preview.Draft `json:"previews"`
		} `json:"hidden_client_tool"`
		DirectClientTool struct {
			Previews []preview.Draft `json:"previews"`
		} `json:"direct_client_tool"`
	}
	data, err := os.ReadFile(filepath.Join(root, "producer-wire.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &producer); err != nil {
		return err
	}
	fallback, err := os.ReadFile(filepath.Join(root, "assets", "fallback.png"))
	if err != nil {
		return err
	}
	var shapes []string
	for _, item := range []struct {
		name   string
		drafts []preview.Draft
	}{
		{"server_tool", producer.ServerTool.Previews},
		{"hidden_client_tool", producer.HiddenClientTool.Previews},
		{"direct_client_tool", producer.DirectClientTool.Previews},
	} {
		service, store, err := fixtureService()
		if err != nil {
			return err
		}
		service.AssetLoader = staticAssetLoader{data: fallback, mime: "image/png"}
		result := service.Produce(context.Background(), preview.ProduceRequest{
			SessionID: "session_fixture", TurnID: "turn_fixture", CallID: "call_fixture",
			ProducerID: item.name, Capability: "ui-adaptation", Drafts: item.drafts,
			Assets: []assets.Ref{{ID: "asset_fixture_fallback", MIMEType: "image/png"}},
		})
		if result.Fatal != nil || len(result.Artifacts) != 1 {
			if len(result.Rejected) > 0 {
				return fmt.Errorf("%s produce rejected: %s (%s)", item.name, result.Rejected[0].Err.Code, result.Rejected[0].Err.Message)
			}
			return fmt.Errorf("%s produce result = %+v", item.name, result)
		}
		artifact := result.Artifacts[0]
		if artifact.Status != preview.StatusReady || artifact.Kind != preview.KindInteractiveHTML {
			return fmt.Errorf("%s artifact = %s/%s", item.name, artifact.Status, artifact.Kind)
		}
		if _, err := store.GetArtifact(context.Background(), artifact.SessionID, artifact.ArtifactID, artifact.Revision); err != nil {
			return err
		}
		shapes = append(shapes, artifactShape(artifact))
	}
	for i := 1; i < len(shapes); i++ {
		if shapes[i] != shapes[0] {
			return fmt.Errorf("producer artifact shape differs: %s != %s", shapes[i], shapes[0])
		}
	}
	return nil
}

func runSecurityCases(root string, tc ManifestCase) error {
	for _, name := range tc.Files {
		path := filepath.Join(root, filepath.FromSlash(name))
		switch name {
		case "negative/navigation-attempt.html", "negative/network-attempt.html", "negative/bridge-attempt.html":
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, perr := preview.MaterializeViewer("fixture", string(data)); perr == nil || perr.Code != preview.CodeSecurityViolation {
				return fmt.Errorf("%s viewer error = %+v", name, perr)
			}
		case "negative/asset-path-rejection.json":
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var wrapper struct {
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(data, &wrapper); err != nil {
				return err
			}
			var draft preview.Draft
			if err := json.Unmarshal(wrapper.Input, &draft); err == nil {
				return fmt.Errorf("asset path rejection accepted forbidden fields")
			}
		case "negative/digest-mismatch.json":
			if err := preview.ValidateViewerBytes([]byte("viewer"), 6, "sha256:"+strings.Repeat("0", 64)); err == nil || err.Code != preview.CodeDigestMismatch {
				return fmt.Errorf("digest mismatch error = %+v", err)
			}
		case "negative/invalid-utf8.bin":
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, perr := preview.MaterializeViewer("fixture", string(data)); perr == nil || perr.Code != preview.CodeInvalidArtifact {
				return fmt.Errorf("invalid utf8 error = %+v", perr)
			}
		case "negative/forged-producer-identity.json":
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var wrapper struct {
				Preview json.RawMessage `json:"preview"`
			}
			if err := json.Unmarshal(data, &wrapper); err != nil {
				return err
			}
			var draft preview.Draft
			if err := json.Unmarshal(wrapper.Preview, &draft); err == nil {
				return fmt.Errorf("forged producer identity accepted")
			}
		case "negative/viewer-error-response.json":
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var response struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
				Data struct {
					HTTPStatus int `json:"http_status"`
					Error      struct {
						Code string `json:"code"`
					} `json:"error"`
				} `json:"data"`
			}
			if err := json.Unmarshal(data, &response); err != nil {
				return err
			}
			if response.Code != 42200 || response.Msg != "preview_digest_mismatch" || response.Data.HTTPStatus != 422 || response.Data.Error.Code != "preview_digest_mismatch" {
				return fmt.Errorf("viewer error response = %+v", response)
			}
		}
	}
	return nil
}

func runSizeBoundary(root string, tc ManifestCase) error {
	data, err := os.ReadFile(filepath.Join(root, "negative", "size-cases.json"))
	if err != nil {
		return err
	}
	var fixture struct {
		Cases []struct {
			Bytes    int64  `json:"bytes"`
			File     string `json:"file"`
			SHA256   string `json:"sha256"`
			Expected string `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err
	}
	for _, c := range fixture.Cases {
		file, err := os.ReadFile(filepath.Join(root, "negative", c.File))
		if err != nil {
			return err
		}
		if int64(len(file)) != c.Bytes || !preview.DigestMatches(c.SHA256, file) {
			return fmt.Errorf("%s bytes/digest mismatch", c.File)
		}
		validateErr := preview.ValidateViewerBytes(file, int64(len(file)), c.SHA256)
		switch c.Expected {
		case "accepted":
			if validateErr != nil {
				return validateErr
			}
		case "preview_viewer_too_large":
			var perr *preview.Error
			if !errors.As(validateErr, &perr) || perr.Code != preview.CodeViewerTooLarge {
				return fmt.Errorf("%s error = %+v", c.File, validateErr)
			}
		}
	}
	return nil
}

func runResolver(root string, tc ManifestCase) error {
	artifact, err := loadArtifact(filepath.Join(root, "ready-interactive.json"))
	if err != nil {
		return err
	}
	viewer, err := os.ReadFile(filepath.Join(root, "viewer.html"))
	if err != nil {
		return err
	}
	asset, err := os.ReadFile(filepath.Join(root, "assets", "fallback.png"))
	if err != nil {
		return err
	}
	service, store, err := fixtureService()
	if err != nil {
		return err
	}
	namespace := preview.Namespace{SessionID: artifact.SessionID}
	if err := store.ImportArtifact(context.Background(), namespace, artifact, viewer, map[string][]byte{"asset_fixture_fallback": asset}); err != nil {
		return err
	}
	viewerResource, err := service.Resolve(artifact.SessionID, artifact.ArtifactID, artifact.Revision)
	if err != nil {
		return err
	}
	assetResource, err := service.ResolveContent(artifact.SessionID, artifact.ArtifactID, artifact.Revision, "asset_fixture_fallback")
	if err != nil {
		return err
	}
	if !bytes.Equal(viewerResource.Data, viewer) || !bytes.Equal(assetResource.Data, asset) {
		return fmt.Errorf("in-process resolver bytes differ from fixture bytes")
	}
	var contract struct {
		Viewer struct {
			ExpectedMIMEType  string `json:"expected_mime_type"`
			ExpectedSizeBytes int64  `json:"expected_size_bytes"`
			ExpectedSHA256    string `json:"expected_sha256"`
		} `json:"viewer"`
		Asset struct {
			ExpectedMIMEType  string `json:"expected_mime_type"`
			ExpectedSizeBytes int64  `json:"expected_size_bytes"`
			ExpectedSHA256    string `json:"expected_sha256"`
		} `json:"asset"`
	}
	data, err := os.ReadFile(filepath.Join(root, "resolver-contract.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		return err
	}
	if viewerResource.MIMEType != contract.Viewer.ExpectedMIMEType || viewerResource.SizeBytes != contract.Viewer.ExpectedSizeBytes || viewerResource.SHA256 != contract.Viewer.ExpectedSHA256 {
		return fmt.Errorf("viewer resolver metadata mismatch")
	}
	if assetResource.MIMEType != contract.Asset.ExpectedMIMEType || assetResource.SizeBytes != contract.Asset.ExpectedSizeBytes || assetResource.SHA256 != contract.Asset.ExpectedSHA256 {
		return fmt.Errorf("asset resolver metadata mismatch")
	}
	return nil
}

func fixtureService() (*preview.Service, *preview.FSStore, error) {
	dir, err := os.MkdirTemp("", "preview-fixture-")
	if err != nil {
		return nil, nil, err
	}
	store, err := preview.NewFSStore(dir)
	if err != nil {
		return nil, nil, err
	}
	service := preview.NewService(store)
	service.Now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	return service, store, nil
}

type staticAssetLoader struct {
	data []byte
	mime string
}

func (l staticAssetLoader) Load(context.Context, string, assets.Ref) ([]byte, string, error) {
	return append([]byte(nil), l.data...), l.mime, nil
}

func loadArtifact(path string) (preview.Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return preview.Artifact{}, err
	}
	var artifact preview.Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return preview.Artifact{}, err
	}
	return artifact, nil
}

func RevisionSHA(artifact preview.Artifact) (string, error) { return preview.RevisionSHA256(artifact) }

func decodeExpected(raw map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		var decoded any
		if json.Unmarshal(value, &decoded) == nil {
			out[key] = decoded
		}
	}
	return out
}

func int64From(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func artifactShape(artifact preview.Artifact) string {
	keys := []string{
		"status=" + string(artifact.Status),
		"kind=" + string(artifact.Kind),
		fmt.Sprintf("assets=%d", len(artifact.Assets)),
		fmt.Sprintf("viewer=%t", artifact.Viewer != nil),
		fmt.Sprintf("fallback=%t", artifact.Fallback != nil),
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
