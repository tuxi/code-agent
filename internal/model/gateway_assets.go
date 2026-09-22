package model

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// AssetUploadScoper returns an opaque, non-secret identity partition. The
// Runtime never persists credentials; a new JWT naturally forms a new partition.
// It partitions the Gateway image-input capability cache and the asset-ref
// release outbox; it no longer participates in any file upload.
type AssetUploadScoper interface {
	AssetUploadScope(ctx context.Context) string
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
