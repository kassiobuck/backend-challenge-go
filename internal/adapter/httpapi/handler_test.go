package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type readinessStub map[string]string

func (r readinessStub) Check(context.Context) map[string]string { return r }

type tokenVerifierStub struct {
	principal port.Principal
	err       error
}

func (v tokenVerifierStub) Verify(context.Context, string) (port.Principal, error) {
	return v.principal, v.err
}

func TestHealthRoutes(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		checker    ReadinessChecker
		statusCode int
	}{
		{name: "liveness", method: http.MethodGet, path: "/health/live", statusCode: http.StatusOK},
		{
			name: "ready when dependencies are healthy", method: http.MethodGet, path: "/health/ready",
			checker: readinessStub{"postgresql": "ok", "sqs": "ok"}, statusCode: http.StatusOK,
		},
		{
			name: "not ready when a dependency is unavailable", method: http.MethodGet, path: "/health/ready",
			checker:    readinessStub{"postgresql": "ok", "sqs": "unavailable"},
			statusCode: http.StatusServiceUnavailable,
		},
		{name: "unsupported method", method: http.MethodPost, path: "/health/live", statusCode: http.StatusMethodNotAllowed},
	}
	logger := slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(test.checker, logger, nil, nil)
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.statusCode {
				t.Fatalf("status code = %d, want %d", response.Code, test.statusCode)
			}
			if test.statusCode != http.StatusMethodNotAllowed &&
				response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestBusinessRoutesRequireAuthenticationAndEnforceIdentity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil))
	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		verifier   port.TokenVerifier
		body       string
		wantStatus int
	}{
		{
			name:   "wallet endpoint rejects missing credentials",
			method: http.MethodGet, path: "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "provider cannot call internal wallet endpoint",
			method: http.MethodGet, path: "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37",
			token: "valid", verifier: tokenVerifierStub{principal: testPrincipal("provider-a", "wager:read")},
			wantStatus: http.StatusForbidden,
		},
		{
			name:   "provider cannot query another provider path",
			method: http.MethodGet, path: "/providers/provider-b/wagering/transactions/external-1",
			token: "valid", verifier: tokenVerifierStub{principal: testPrincipal("provider-a", "wager:read")},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "body cannot override authenticated provider identity",
			method: http.MethodPost, path: "/wagering/transactions",
			token: "valid", verifier: tokenVerifierStub{principal: testPrincipal("provider-a", "wager:write")},
			body: `{"providerId":"provider-b"}`, wantStatus: http.StatusForbidden,
		},
		{
			name:   "malformed bearer token is rejected",
			method: http.MethodGet, path: "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37",
			token: "invalid", verifier: tokenVerifierStub{err: errors.New("invalid")},
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(readinessStub{}, logger, nil, test.verifier)
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func testPrincipal(providerID string, permissions ...string) port.Principal {
	values := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		values[permission] = struct{}{}
	}
	return port.Principal{Subject: "test-subject", ProviderID: providerID, Permissions: values}
}
