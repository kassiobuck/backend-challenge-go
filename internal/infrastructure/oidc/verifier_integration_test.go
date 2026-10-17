package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestVerifierAgainstRealOIDCProvider(t *testing.T) {
	issuer := os.Getenv("TEST_OIDC_ISSUER")
	clientID := os.Getenv("TEST_OIDC_CLIENT_ID")
	clientSecret := os.Getenv("TEST_OIDC_CLIENT_SECRET")
	audience := os.Getenv("TEST_OIDC_AUDIENCE")
	providerID := os.Getenv("TEST_OIDC_PROVIDER_ID")
	if issuer == "" || clientID == "" || clientSecret == "" || audience == "" || providerID == "" {
		t.Skip("TEST_OIDC_ISSUER, client credentials, audience, and provider id are required for real IdP integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	token := requestClientCredentialsToken(t, ctx, issuer, clientID, clientSecret)
	verifier, err := NewVerifier(ctx, issuer, audience, os.Getenv("TEST_OIDC_INTERNAL_URL"))
	if err != nil {
		t.Fatalf("initialize real OIDC verifier: %v", err)
	}
	principal, err := verifier.Verify(ctx, token)
	if err != nil {
		t.Fatalf("verify real IdP access token: %v", err)
	}
	if principal.ProviderID != providerID {
		t.Fatalf("provider claim = %q, want %q", principal.ProviderID, providerID)
	}
	if !principal.HasPermission("wager:write") {
		t.Fatal("provider access token lacks wager:write permission")
	}
}

func requestClientCredentialsToken(t *testing.T, ctx context.Context, issuer, clientID, secret string) string {
	t.Helper()
	endpoint := strings.TrimRight(issuer, "/") + "/protocol/openid-connect/token"
	form := url.Values{"grant_type": {"client_credentials"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("create client credentials request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(clientID, secret)
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request token from real IdP: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("real IdP token endpoint status = %d", response.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode real IdP token response: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal(fmt.Errorf("real IdP response contained no access token"))
	}
	return result.AccessToken
}
