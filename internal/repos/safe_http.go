package repos

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const publicHTTPSProtocol = "codeagent-public-https"

var installPublicHTTPSOnce sync.Once

// installPublicHTTPSTransport registers a private go-git protocol instead of
// replacing the process-wide https transport. Existing git tools may legitimately
// talk to private remotes; only the unauthenticated public-clone API gets the SSRF
// restrictions below.
func installPublicHTTPSTransport() {
	installPublicHTTPSOnce.Do(func() {
		client := &http.Client{
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           publicDialContext,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				return validatePublicURL(req.Context(), req.URL)
			},
		}
		transportclient.InstallProtocol(publicHTTPSProtocol, publicHTTPTransport{
			delegate: githttp.NewClientWithOptions(client, &githttp.ClientOptions{
				RedirectPolicy: githttp.FollowRedirects,
			}),
		})
	})
}

type publicHTTPTransport struct {
	delegate transport.Transport
}

func (t publicHTTPTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	if auth != nil || ep.User != "" || ep.Password != "" {
		return nil, errors.New("credentials are not allowed for public clone")
	}
	copy := *ep
	copy.Protocol = "https"
	return t.delegate.NewUploadPackSession(&copy, nil)
}

func (t publicHTTPTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return nil, errors.New("public clone transport is read-only")
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid destination address: %w", err)
	}
	addrs, err := resolvePublicHost(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var errs []error
	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

func validatePublicURL(ctx context.Context, u *url.URL) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return errors.New("redirect target must use https")
	}
	if u.User != nil || u.Fragment != "" {
		return errors.New("credentials and fragments are not allowed")
	}
	if u.RawQuery != "" {
		query := u.Query()
		if len(query) != 1 || len(query["service"]) != 1 || query.Get("service") != "git-upload-pack" {
			return errors.New("redirect query parameters are not allowed")
		}
	}
	if u.Hostname() == "" {
		return errors.New("destination host is required")
	}
	_, err := resolvePublicHost(ctx, u.Hostname())
	return err
}

func resolvePublicHost(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("destination host %q is not public", host)
	}
	if strings.Contains(host, "%") {
		return nil, errors.New("scoped IP addresses are not allowed")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if !isPublicIP(addr) {
			return nil, fmt.Errorf("destination address %s is not public", addr)
		}
		return []netip.Addr{addr}, nil
	}
	resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("destination host %q has no addresses", host)
	}
	result := make([]netip.Addr, 0, len(resolved))
	for _, addr := range resolved {
		addr = addr.Unmap()
		if !isPublicIP(addr) {
			return nil, fmt.Errorf("destination host %q resolves to non-public address %s", host, addr)
		}
		result = append(result, addr)
	}
	return result, nil
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func isPublicIP(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
