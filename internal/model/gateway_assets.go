package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// AssetUpload is a local, already-validated file that must become a
// Gateway-owned reference. It is never serialized into a chat request.
type AssetUpload struct {
	Path         string
	AssetClass   string
	AssetKind    string
	BusinessType string
	Filename     string
	MIMEType     string
	SizeBytes    int64
	SHA256       string
}

// AssetUploader is the small capability the agent loop needs. Code-Agent does
// not know STS, OSS, or VLM details; it asks its Gateway-backed provider to turn
// a local file into an ownership-bound GatewayAssetRef.
type AssetUploader interface {
	UploadAsset(ctx context.Context, upload AssetUpload) (GatewayAssetRef, error)
}

// AssetUploadScoper returns an opaque, non-secret identity partition for an
// upload cache. The Runtime never persists credentials; a new JWT naturally
// forms a new cache partition and Gateway remains the final ownership check.
type AssetUploadScoper interface {
	AssetUploadScope(ctx context.Context) string
}

type gatewayUploadInitRequest struct {
	AssetClass   string `json:"asset_class"`
	AssetKind    string `json:"asset_kind"`
	BusinessType string `json:"business_type"`
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type"`
	SizeBytes    int64  `json:"size_bytes"`
}

type gatewayUploadInitResponse struct {
	AssetID   int64  `json:"asset_id"`
	UploadID  string `json:"upload_id"`
	Bucket    string `json:"bucket"`
	Endpoint  string `json:"endpoint"`
	ObjectKey string `json:"object_key"`
	STS       struct {
		AccessKeyID     string `json:"access_key_id"`
		AccessKeySecret string `json:"access_key_secret"`
		SecurityToken   string `json:"security_token"`
	} `json:"sts"`
}

type gatewayUploadCompleteRequest struct {
	AssetID  int64  `json:"asset_id"`
	UploadID string `json:"upload_id"`
	OSSKey   string `json:"oss_key"`
	SHA256   string `json:"sha256,omitempty"`
}

type gatewayEnvelope[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// GatewayObjectUploader makes the STS object-store step mockable. Production
// uses aliOSSObjectUploader; tests can substitute a recorder without secrets.
type GatewayObjectUploader interface {
	Upload(ctx context.Context, init gatewayUploadInitResponse, localPath, mimeType string) error
}

type aliOSSObjectUploader struct{}

func (aliOSSObjectUploader) Upload(_ context.Context, init gatewayUploadInitResponse, localPath, mimeType string) error {
	client, err := oss.New(init.Endpoint, init.STS.AccessKeyID, init.STS.AccessKeySecret, oss.SecurityToken(init.STS.SecurityToken))
	if err != nil {
		return fmt.Errorf("create oss client: %w", err)
	}
	bucket, err := client.Bucket(init.Bucket)
	if err != nil {
		return fmt.Errorf("open oss bucket: %w", err)
	}
	options := []oss.Option{}
	if mimeType != "" {
		options = append(options, oss.ContentType(mimeType))
	}
	if err := bucket.PutObjectFromFile(init.ObjectKey, localPath, options...); err != nil {
		return fmt.Errorf("upload oss object: %w", err)
	}
	return nil
}

// ObjectUploader is optional test wiring. Nil selects the Aliyun STS uploader.
// It is intentionally on the Gateway provider, where the JWT and API base URL
// already live, rather than in the agent loop.
func (p *OpenAICompatibleProvider) gatewayObjectUploader() GatewayObjectUploader {
	if p.ObjectUploader != nil {
		return p.ObjectUploader
	}
	return aliOSSObjectUploader{}
}

// UploadAsset performs Gateway init -> STS direct upload -> complete. Neither
// STS material nor the returned object URL is retained in the model transcript.
func (p *OpenAICompatibleProvider) UploadAsset(ctx context.Context, upload AssetUpload) (GatewayAssetRef, error) {
	if strings.TrimSpace(upload.Path) == "" || strings.TrimSpace(upload.Filename) == "" {
		return GatewayAssetRef{}, fmt.Errorf("gateway asset upload: path and filename are required")
	}
	info, err := os.Stat(upload.Path)
	if err != nil || info.IsDir() {
		return GatewayAssetRef{}, fmt.Errorf("gateway asset upload: invalid local file")
	}
	if upload.SizeBytes <= 0 {
		upload.SizeBytes = info.Size()
	}
	initReq := gatewayUploadInitRequest{
		AssetClass: upload.AssetClass, AssetKind: upload.AssetKind, BusinessType: upload.BusinessType,
		Filename: upload.Filename, ContentType: upload.MIMEType, SizeBytes: upload.SizeBytes,
	}
	var init gatewayUploadInitResponse
	if err := p.gatewayJSON(ctx, http.MethodPost, "/uploads/init", initReq, &init); err != nil {
		return GatewayAssetRef{}, err
	}
	if init.AssetID <= 0 || init.UploadID == "" || init.ObjectKey == "" {
		return GatewayAssetRef{}, fmt.Errorf("gateway asset upload: invalid init response")
	}
	if err := p.gatewayObjectUploader().Upload(ctx, init, upload.Path, upload.MIMEType); err != nil {
		return GatewayAssetRef{}, err
	}
	var ignored json.RawMessage
	if err := p.gatewayJSON(ctx, http.MethodPost, "/uploads/complete", gatewayUploadCompleteRequest{
		AssetID: init.AssetID, UploadID: init.UploadID, OSSKey: init.ObjectKey, SHA256: upload.SHA256,
	}, &ignored); err != nil {
		return GatewayAssetRef{}, err
	}
	return GatewayAssetRef{AssetID: init.AssetID, SHA256: upload.SHA256, Kind: upload.AssetKind, MIMEType: upload.MIMEType, Filename: upload.Filename}, nil
}

func (p *OpenAICompatibleProvider) AssetUploadScope(ctx context.Context) string {
	secret := p.APIKey
	if p.Credential != nil {
		if cred, err := p.Credential.Resolve(ctx, p.CredentialTarget); err == nil && !cred.IsZero() {
			secret = cred.Secret
		}
	}
	sum := sha256.Sum256([]byte(p.BaseURL + "\x00" + secret))
	return fmt.Sprintf("gateway:%x", sum[:8])
}

func (p *OpenAICompatibleProvider) gatewayJSON(ctx context.Context, method, endpoint string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	url, err := gatewayURL(p.BaseURL, endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := p.applyAuth(ctx, req); err != nil {
		return err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope gatewayEnvelope[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode gateway asset response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code != 0 {
		return fmt.Errorf("gateway asset request failed: %s", envelope.Message)
	}
	if output != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			return fmt.Errorf("decode gateway asset data: %w", err)
		}
	}
	return nil
}

func gatewayURL(baseURL, endpoint string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid gateway base url")
	}
	p := strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(p, "/agent") {
		p = strings.TrimSuffix(p, "/agent")
	}
	if !strings.HasSuffix(p, "/api/v1") {
		return "", fmt.Errorf("gateway base url must end in /api/v1/agent")
	}
	u.Path = path.Join(p, endpoint)
	return u.String(), nil
}
