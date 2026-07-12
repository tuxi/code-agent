package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type recordingObjectUploader struct {
	init gatewayUploadInitResponse
	path string
	mime string
}

func (u *recordingObjectUploader) Upload(_ context.Context, init gatewayUploadInitResponse, localPath, mimeType string) error {
	u.init, u.path, u.mime = init, localPath, mimeType
	return nil
}

func TestGatewayAssetUploadUsesJWTSTSAndReturnsReference(t *testing.T) {
	var complete gatewayUploadCompleteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/uploads/init":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"asset_id":42,"upload_id":"up_1","bucket":"bucket","endpoint":"oss.example.test","object_key":"private/screenshot.png","sts":{"access_key_id":"ak","access_key_secret":"sk","security_token":"sts"}}}`))
		case "/api/v1/uploads/complete":
			if err := json.NewDecoder(r.Body).Decode(&complete); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"asset_id":42}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "screenshot.png")
	if err := os.WriteFile(file, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploader := &recordingObjectUploader{}
	p := NewOpenAICompatibleProviderWithKey(srv.URL+"/api/v1/agent", "jwt")
	p.ObjectUploader = uploader
	ref, err := p.UploadAsset(context.Background(), AssetUpload{
		Path: file, AssetClass: "agent_screenshot", AssetKind: "image", BusinessType: "agent_screenshot",
		Filename: "screenshot.png", MIMEType: "image/png", SHA256: "sha",
	})
	if err != nil {
		t.Fatalf("UploadAsset: %v", err)
	}
	if ref.AssetID != 42 || ref.SHA256 != "sha" || ref.MIMEType != "image/png" {
		t.Fatalf("GatewayAssetRef = %+v", ref)
	}
	if uploader.init.ObjectKey != "private/screenshot.png" || uploader.path != file || uploader.mime != "image/png" {
		t.Fatalf("STS upload = %+v path=%q mime=%q", uploader.init, uploader.path, uploader.mime)
	}
	if complete.AssetID != 42 || complete.UploadID != "up_1" || complete.OSSKey != "private/screenshot.png" || complete.SHA256 != "sha" {
		t.Fatalf("complete = %+v", complete)
	}
}
