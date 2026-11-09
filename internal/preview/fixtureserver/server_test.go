package fixtureserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/preview"
	"code-agent/internal/preview/fixturetest"
)

func TestServerBindsLoopbackAndRequiresBearer(t *testing.T) {
	srv := startTestServer(t)
	parsed, err := url.Parse(srv.BaseURL())
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("server host = %q, want loopback", parsed.Hostname())
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || port == "" || port == "0" {
		t.Fatalf("temporary listener = %q, %v", parsed.Host, err)
	}
	if len([]byte(srv.Ready().BearerToken)) < 32 {
		t.Fatalf("bearer token is too short")
	}
	other := startTestServer(t)
	if srv.Ready().BearerToken == other.Ready().BearerToken {
		t.Fatal("two fixture servers received the same bearer token")
	}

	body, resp := getFixture(t, srv, "/v1/runtime/capabilities", false)
	if resp.StatusCode != http.StatusUnauthorized || !bytes.Contains(body, []byte("runtime_auth_required")) {
		t.Fatalf("unauthorized response = %d %s", resp.StatusCode, body)
	}
	_, resp = getFixture(t, srv, "/v1/runtime/capabilities", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized capability status = %d", resp.StatusCode)
	}
}

func TestCapabilitySnapshotAdvertisesFullPreviewSurface(t *testing.T) {
	srv := startTestServer(t)
	var snapshot struct {
		Capabilities struct {
			PreviewContractV1        bool `json:"preview_contract_v1"`
			PreviewInteractiveHTMLV1 bool `json:"preview_interactive_html_v1"`
			PreviewStaticAssetV1     bool `json:"preview_static_asset_v1"`
			PreviewReplayV1          bool `json:"preview_replay_v1"`
		} `json:"capabilities"`
	}
	decodeEnvelope(t, srv, "/v1/runtime/capabilities", &snapshot)
	caps := snapshot.Capabilities
	if !caps.PreviewContractV1 || !caps.PreviewInteractiveHTMLV1 || !caps.PreviewStaticAssetV1 || !caps.PreviewReplayV1 {
		t.Fatalf("preview capability snapshot = %+v", caps)
	}
}

func TestViewerAndAssetMatchFrozenContract(t *testing.T) {
	srv := startTestServer(t)
	root := testFixtureRoot(t)
	contract := loadResolverContract(t, root)

	catalog := loadCatalog(t, srv)
	ready := findScenario(t, catalog, "ready-interactive")
	if ready.ViewerURL != "/v1/conversations/session_fixture/previews/preview_fixture_interactive/revisions/1/viewer" {
		t.Fatalf("viewer URL = %q", ready.ViewerURL)
	}
	if len(ready.Assets) != 1 || ready.Assets[0].URL != "/v1/conversations/session_fixture/previews/preview_fixture_interactive/revisions/1/assets/asset_fixture_fallback/content" {
		t.Fatalf("asset URLs = %+v", ready.Assets)
	}

	viewerBody, viewerResp := getFixture(t, srv, ready.ViewerURL, true)
	viewerFixture := readFile(t, filepath.Join(root, "viewer.html"))
	if viewerResp.StatusCode != http.StatusOK || !bytes.Equal(viewerBody, viewerFixture) {
		t.Fatalf("viewer response = %d, %d bytes", viewerResp.StatusCode, len(viewerBody))
	}
	if got := viewerResp.Header.Get("Content-Type"); got != contract.Viewer.ExpectedMIMEType {
		t.Fatalf("viewer MIME = %q", got)
	}
	if got := viewerResp.Header.Get("Content-Length"); got != fmt.Sprint(contract.Viewer.ExpectedSizeBytes) {
		t.Fatalf("viewer size header = %q", got)
	}
	if got := viewerResp.Header.Get("ETag"); got != `"`+contract.Viewer.ExpectedSHA256+`"` {
		t.Fatalf("viewer ETag = %q", got)
	}
	if got := viewerResp.Header.Get("Content-Security-Policy"); got != preview.ViewerContentSecurityPolicy {
		t.Fatalf("viewer CSP = %q", got)
	}

	assetBody, assetResp := getFixture(t, srv, ready.Assets[0].URL, true)
	assetFixture := readFile(t, filepath.Join(root, "assets", "fallback.png"))
	if assetResp.StatusCode != http.StatusOK || !bytes.Equal(assetBody, assetFixture) {
		t.Fatalf("asset response = %d, %d bytes", assetResp.StatusCode, len(assetBody))
	}
	if got := assetResp.Header.Get("Content-Type"); got != contract.Asset.ExpectedMIMEType {
		t.Fatalf("asset MIME = %q", got)
	}
	if got := assetResp.Header.Get("Content-Length"); got != fmt.Sprint(contract.Asset.ExpectedSizeBytes) {
		t.Fatalf("asset size header = %q", got)
	}
	if got := assetResp.Header.Get("ETag"); got != `"`+contract.Asset.ExpectedSHA256+`"` {
		t.Fatalf("asset ETag = %q", got)
	}
	decoded, format, err := image.Decode(bytes.NewReader(assetBody))
	if err != nil {
		t.Fatalf("decode fallback PNG: %v", err)
	}
	if format != "png" || decoded.Bounds().Dx() != 1200 || decoded.Bounds().Dy() != 800 {
		t.Fatalf("fallback image = %s %v", format, decoded.Bounds())
	}

	revisionTwoAsset := "/v1/conversations/session_fixture/previews/preview_fixture_interactive/revisions/2/assets/asset_fixture_fallback/content"
	revisionTwoBody, revisionTwoResp := getFixture(t, srv, revisionTwoAsset, true)
	if revisionTwoResp.StatusCode != http.StatusOK || !bytes.Equal(revisionTwoBody, assetFixture) {
		t.Fatalf("revision 2 fallback response = %d, %d bytes", revisionTwoResp.StatusCode, len(revisionTwoBody))
	}
}

func TestCatalogCoversFrozenScenariosAndArtifacts(t *testing.T) {
	srv := startTestServer(t)
	catalog := loadCatalog(t, srv)
	if len(catalog.Scenarios) != 16 {
		t.Fatalf("catalog scenarios = %d, want 16", len(catalog.Scenarios))
	}
	required := []string{
		"ready-interactive", "static-image", "pending", "failed-with-fallback",
		"failed-without-fallback", "unavailable", "unavailable-with-fallback",
		"expiry-derived", "revision-replacement", "duplicate-revision", "revision-conflict",
	}
	states := map[string]preview.PresentationState{
		"ready-interactive":         preview.StateInteractiveReady,
		"static-image":              preview.StateStaticFallback,
		"pending":                   preview.StateLoading,
		"failed-with-fallback":      preview.StateStaticFallback,
		"failed-without-fallback":   preview.StateUnavailable,
		"unavailable":               preview.StateUnavailable,
		"unavailable-with-fallback": preview.StateStaticFallback,
		"expiry-derived":            preview.StateExpired,
	}
	now, err := time.Parse(time.RFC3339, catalog.EvaluationTime)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range required {
		scenario := findScenario(t, catalog, id)
		if scenario.ArtifactURL == "" {
			t.Fatalf("%s has no artifact URL", id)
		}
		body, resp := getFixture(t, srv, scenario.ArtifactURL, true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s artifact status = %d", id, resp.StatusCode)
		}
		var artifact preview.Artifact
		if err := json.Unmarshal(body, &artifact); err != nil {
			t.Fatalf("%s artifact decode: %v", id, err)
		}
		if err := artifact.Validate(); err != nil {
			t.Fatalf("%s artifact validation: %v", id, err)
		}
		if want := states[id]; want != "" {
			got := preview.DerivePresentation(preview.PresentationInput{
				Artifact: artifact, Capabilities: preview.Negotiate(
					preview.FullRuntimeFlags().List(), preview.FullRuntimeFlags().List(), false,
				), Now: now,
			})
			if got != want {
				t.Fatalf("%s presentation = %s, want %s", id, got, want)
			}
		}
	}
}

func TestExpiryReturnsGone(t *testing.T) {
	srv := startTestServer(t)
	catalog := loadCatalog(t, srv)
	expiry := findScenario(t, catalog, "expiry-derived")
	if expiry.ViewerURL == "" {
		t.Fatal("expiry fixture has no viewer URL")
	}
	body, resp := getFixture(t, srv, expiry.ViewerURL, true)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired viewer status = %d %s", resp.StatusCode, body)
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			HTTPStatus int `json:"http_status"`
			Error      struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != 41000 || envelope.Data.HTTPStatus != 410 || envelope.Data.Error.Code != string(preview.CodeExpired) {
		t.Fatalf("expiry error = %+v", envelope)
	}
}

func TestReplayFixturesPreserveRevisionSemantics(t *testing.T) {
	srv := startTestServer(t)
	catalog := loadCatalog(t, srv)
	tests := []struct {
		scenario string
		check    func(*testing.T, preview.ReplayResult)
	}{
		{"revision-replacement", func(t *testing.T, result preview.ReplayResult) {
			if result.Error != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Revision != 2 || result.Artifacts[0].Status != preview.StatusFailed {
				t.Fatalf("replacement replay = %+v", result)
			}
		}},
		{"duplicate-revision", func(t *testing.T, result preview.ReplayResult) {
			if result.Error != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Revision != 1 || result.Artifacts[0].Status != preview.StatusReady {
				t.Fatalf("duplicate replay = %+v", result)
			}
		}},
		{"revision-conflict", func(t *testing.T, result preview.ReplayResult) {
			if result.Error == nil || result.Error.Code != preview.CodeRevisionConflict || len(result.Artifacts) != 1 || result.Artifacts[0].Revision != 1 {
				t.Fatalf("conflict replay = %+v", result)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.scenario, func(t *testing.T) {
			scenario := findScenario(t, catalog, tc.scenario)
			rawFirst, rawResp := getFixture(t, srv, scenario.EventsURL, true)
			rawSecond, _ := getFixture(t, srv, scenario.EventsURL, true)
			if rawResp.StatusCode != http.StatusOK || !bytes.Equal(rawFirst, rawSecond) {
				t.Fatalf("raw replay fixture is not stable: %d", rawResp.StatusCode)
			}
			tc.check(t, replayRawEvents(t, rawFirst))

			httpEvents := loadHTTPReplayEvents(t, srv, scenario.ReplayURL)
			tc.check(t, preview.ApplyReplay(httpEvents))
		})
	}
}

func TestFrozenFixtureSuite(t *testing.T) {
	report, err := fixturetest.Run(testFixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.Contract != preview.ContractID || len(report.Scenarios) != 16 {
		t.Fatalf("fixture report = %+v", report)
	}
}

type resolverContract struct {
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

func startTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := Start(testFixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown fixture server: %v", err)
		}
	})
	return srv
}

func testFixtureRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
}

func getFixture(t *testing.T, srv *Server, path string, auth bool) ([]byte, *http.Response) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.BaseURL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", srv.Ready().Authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body, resp
}

func decodeEnvelope(t *testing.T, srv *Server, path string, out any) {
	t.Helper()
	body, resp := getFixture(t, srv, path, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status = %d %s", path, resp.StatusCode, body)
	}
	envelope := struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != 0 {
		t.Fatalf("%s envelope = %+v", path, envelope)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		t.Fatal(err)
	}
}

func loadCatalog(t *testing.T, srv *Server) Catalog {
	t.Helper()
	var catalog Catalog
	decodeEnvelope(t, srv, "/v1/preview-fixtures", &catalog)
	return catalog
}

func findScenario(t *testing.T, catalog Catalog, id string) Scenario {
	t.Helper()
	for _, scenario := range catalog.Scenarios {
		if scenario.ID == id {
			return scenario
		}
	}
	t.Fatalf("scenario %q not found", id)
	return Scenario{}
}

func loadHTTPReplayEvents(t *testing.T, srv *Server, path string) []preview.Event {
	t.Helper()
	var frames []json.RawMessage
	decodeEnvelope(t, srv, path, &frames)
	events := make([]preview.Event, 0, len(frames))
	for _, frame := range frames {
		event, err := preview.DecodeEvent(frame)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func replayRawEvents(t *testing.T, raw []byte) preview.ReplayResult {
	t.Helper()
	var fixtures []json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	events := make([]preview.Event, 0, len(fixtures))
	for _, fixture := range fixtures {
		event, err := preview.DecodeEvent(fixture)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return preview.ApplyReplay(events)
}

func loadResolverContract(t *testing.T, root string) resolverContract {
	t.Helper()
	var contract resolverContract
	if err := json.Unmarshal(readFile(t, filepath.Join(root, "resolver-contract.json")), &contract); err != nil {
		t.Fatal(err)
	}
	return contract
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReplayURLsAreRevisionAddressed(t *testing.T) {
	srv := startTestServer(t)
	catalog := loadCatalog(t, srv)
	for _, id := range []string{"revision-replacement", "duplicate-revision", "revision-conflict"} {
		scenario := findScenario(t, catalog, id)
		if !strings.HasPrefix(scenario.ReplayURL, "/v1/conversations/") || !strings.HasSuffix(scenario.ReplayURL, "/events") {
			t.Fatalf("%s replay URL = %q", id, scenario.ReplayURL)
		}
	}
}
