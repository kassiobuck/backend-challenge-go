package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type principalContextKey struct{}

func principalFromContext(ctx context.Context) (port.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(port.Principal)
	return principal, ok
}

func authenticate(
	verifier port.TokenVerifier,
	permission string,
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		scheme, token, found := strings.Cut(authorization, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		if verifier == nil {
			writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is unavailable")
			return
		}
		principal, err := verifier.Verify(r.Context(), strings.TrimSpace(token))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "the bearer token is invalid or expired")
			return
		}
		if permission != "" && !principal.HasPermission(permission) {
			writeError(w, http.StatusForbidden, "forbidden", "the authenticated identity lacks the required permission")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	}
}

func requireProviderPathIdentity(w http.ResponseWriter, r *http.Request, principal port.Principal) bool {
	providerID := strings.TrimSpace(r.PathValue("providerId"))
	if principal.ProviderID == "" || providerID != principal.ProviderID {
		writeError(w, http.StatusNotFound, "not_found", "transaction not found")
		return false
	}
	return true
}

func providerIdentityMatchesBody(bodyProviderID string, principal port.Principal) error {
	if principal.ProviderID == "" || strings.TrimSpace(bodyProviderID) != principal.ProviderID {
		return errors.New("provider identity does not match authenticated identity")
	}
	return nil
}
