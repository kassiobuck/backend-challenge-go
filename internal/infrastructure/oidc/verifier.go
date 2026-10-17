package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

var ErrInvalidToken = errors.New("invalid access token")

type Verifier struct {
	verifier *coreoidc.IDTokenVerifier
	client   *http.Client
}

func NewVerifierFromConfig(cfg config.Config) (*Verifier, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return NewVerifier(ctx, cfg.OIDCIssuerURL, cfg.OIDCAudience, cfg.OIDCInternalURL)
}

func NewVerifier(ctx context.Context, issuer, audience, internalURL string) (*Verifier, error) {
	issuerURL, err := url.Parse(issuer)
	if err != nil || (issuerURL.Scheme != "http" && issuerURL.Scheme != "https") || issuerURL.Host == "" {
		return nil, errors.New("OIDC issuer must be an absolute HTTP or HTTPS URL")
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("OIDC audience must not be empty")
	}
	var internal *url.URL
	if internalURL != "" {
		internal, err = url.Parse(internalURL)
		if err != nil || (internal.Scheme != "http" && internal.Scheme != "https") || internal.Host == "" {
			return nil, errors.New("OIDC internal URL must be an absolute HTTP or HTTPS URL")
		}
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: issuerTransport{
			base:     http.DefaultTransport,
			issuer:   issuerURL,
			internal: internal,
		},
	}
	discoveryCtx := coreoidc.ClientContext(ctx, client)
	provider, err := coreoidc.NewProvider(discoveryCtx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC provider: %w", err)
	}
	return &Verifier{
		verifier: provider.Verifier(&coreoidc.Config{ClientID: audience}),
		client:   client,
	}, nil
}

func (v *Verifier) Verify(ctx context.Context, accessToken string) (port.Principal, error) {
	if v == nil || v.verifier == nil || strings.TrimSpace(accessToken) == "" {
		return port.Principal{}, ErrInvalidToken
	}
	token, err := v.verifier.Verify(ctx, accessToken)
	if err != nil {
		return port.Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var claims struct {
		Subject     string          `json:"sub"`
		ProviderID  string          `json:"provider_id"`
		Permissions json.RawMessage `json:"permissions"`
		Scope       string          `json:"scope"`
	}
	if err := token.Claims(&claims); err != nil {
		return port.Principal{}, fmt.Errorf("%w: read claims: %v", ErrInvalidToken, err)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return port.Principal{}, fmt.Errorf("%w: subject claim is missing", ErrInvalidToken)
	}
	permissions, err := parsePermissions(claims.Permissions)
	if err != nil {
		return port.Principal{}, fmt.Errorf("%w: permissions claim: %v", ErrInvalidToken, err)
	}
	for _, permission := range strings.Fields(claims.Scope) {
		permissions[permission] = struct{}{}
	}
	return port.Principal{
		Subject: strings.TrimSpace(claims.Subject), ProviderID: strings.TrimSpace(claims.ProviderID),
		Permissions: permissions,
	}, nil
}

func parsePermissions(raw json.RawMessage) (map[string]struct{}, error) {
	permissions := make(map[string]struct{})
	if len(raw) == 0 || string(raw) == "null" {
		return permissions, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		for _, permission := range values {
			permission = strings.TrimSpace(permission)
			if permission != "" {
				permissions[permission] = struct{}{}
			}
		}
		return permissions, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("must be a string or array of strings")
	}
	for _, permission := range strings.Fields(value) {
		permissions[permission] = struct{}{}
	}
	return permissions, nil
}

type issuerTransport struct {
	base     http.RoundTripper
	issuer   *url.URL
	internal *url.URL
}

func (t issuerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.base == nil {
		return nil, errors.New("OIDC HTTP transport is not configured")
	}
	if t.internal == nil || !sameHost(request.URL, t.issuer) {
		return t.base.RoundTrip(request)
	}
	rewritten := request.Clone(request.Context())
	rewrittenURL := *request.URL
	rewrittenURL.Scheme = t.internal.Scheme
	rewrittenURL.Host = t.internal.Host
	if t.internal.Path != "" && t.internal.Path != "/" {
		rewrittenURL.Path = strings.TrimRight(t.internal.Path, "/") + "/" + strings.TrimLeft(rewrittenURL.Path, "/")
	}
	rewritten.URL = &rewrittenURL
	if t.internal.Hostname() != t.issuer.Hostname() {
		rewritten.Host = t.internal.Host
	}
	return t.base.RoundTrip(rewritten)
}

func sameHost(candidate, base *url.URL) bool {
	if candidate == nil || base == nil {
		return false
	}
	candidateHost, _, candidateErr := net.SplitHostPort(candidate.Host)
	baseHost, _, baseErr := net.SplitHostPort(base.Host)
	if candidateErr != nil {
		candidateHost = candidate.Hostname()
	}
	if baseErr != nil {
		baseHost = base.Hostname()
	}
	return strings.EqualFold(candidateHost, baseHost)
}
