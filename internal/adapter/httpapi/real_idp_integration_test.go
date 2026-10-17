package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	oidcinfra "github.com/kassiobuck/backend-challenge-go/internal/infrastructure/oidc"
	"github.com/kassiobuck/backend-challenge-go/internal/infrastructure/postgres"
	migrationfiles "github.com/kassiobuck/backend-challenge-go/migrations"
)

func TestHTTPProviderIsolationWithRealOIDCAndPostgres(t *testing.T) {
	issuer := os.Getenv("TEST_OIDC_ISSUER")
	audience := os.Getenv("TEST_OIDC_AUDIENCE")
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	internalClientID := os.Getenv("TEST_OIDC_INTERNAL_CLIENT_ID")
	internalClientSecret := os.Getenv("TEST_OIDC_INTERNAL_CLIENT_SECRET")
	providerAClientID := os.Getenv("TEST_OIDC_PROVIDER_A_CLIENT_ID")
	providerAClientSecret := os.Getenv("TEST_OIDC_PROVIDER_A_CLIENT_SECRET")
	if issuer == "" || audience == "" || databaseURL == "" ||
		internalClientID == "" || internalClientSecret == "" ||
		providerAClientID == "" || providerAClientSecret == "" {
		t.Skip("real HTTP integration requires TEST_DATABASE_URL and real Keycloak client settings")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := realHTTPIntegrationPool(t, ctx, databaseURL)
	verifier, err := oidcinfra.NewVerifier(ctx, issuer, audience, os.Getenv("TEST_OIDC_INTERNAL_URL"))
	if err != nil {
		t.Fatalf("initialize OIDC verifier: %v", err)
	}
	internalToken := realClientToken(t, ctx, issuer, internalClientID, internalClientSecret)
	providerAToken := realClientToken(t, ctx, issuer, providerAClientID, providerAClientSecret)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := application.NewFinancialService(postgres.NewUnitOfWork(pool))
	handler := NewHandler(readinessStub{"postgresql": "ok", "sqs": "ok"}, logger,
		service, verifier)

	walletRequest := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(
		`{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"0.00","currency":"BRL"}}`))
	walletRequest.Header.Set("Authorization", "Bearer "+internalToken)
	walletResponse := httptest.NewRecorder()
	handler.ServeHTTP(walletResponse, walletRequest)
	if walletResponse.Code != http.StatusCreated {
		t.Fatalf("internal wallet creation status = %d body=%s", walletResponse.Code, walletResponse.Body.String())
	}
	var createdWallet struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(walletResponse.Body.Bytes(), &createdWallet); err != nil || createdWallet.ID == "" {
		t.Fatalf("decode created wallet response: id=%q err=%v", createdWallet.ID, err)
	}

	submit := func(token, providerID, externalID string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"providerId": providerID, "externalTransactionId": externalID,
			"playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", "walletId": createdWallet.ID,
			"roundId": "round-integration", "gameId": "game-integration", "kind": "WIN",
			"money": map[string]string{"amount": "1.00", "currency": string(money.BRL)},
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", providerID+":"+externalID)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("provider %s transaction status = %d body=%s", providerID, response.Code, response.Body.String())
		}
	}
	submit(providerAToken, "provider-a", "shared-external-id-a")
	foreignAmount, err := money.ParseAmount("1.00", money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	foreignTransaction, err := service.ProcessWager(ctx, application.ProcessWagerCommand{
		ProviderID: "provider-b", ExternalTransactionID: "shared-external-id-b",
		IdempotencyKey: "provider-b:shared-external-id-b", WalletID: createdWallet.ID,
		PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", RoundID: "round-integration",
		GameID: "game-integration", Kind: "WIN",
		Amount: foreignAmount,
	})
	if err != nil {
		t.Fatalf("seed another provider transaction: %v", err)
	}
	if foreignTransaction.Transaction.ProviderID() != "provider-b" {
		t.Fatalf("foreign transaction provider = %q", foreignTransaction.Transaction.ProviderID())
	}

	getOwn := httptest.NewRequest(http.MethodGet,
		"/providers/provider-a/wagering/transactions/shared-external-id-a", nil)
	getOwn.Header.Set("Authorization", "Bearer "+providerAToken)
	ownResponse := httptest.NewRecorder()
	handler.ServeHTTP(ownResponse, getOwn)
	if ownResponse.Code != http.StatusOK {
		t.Fatalf("provider own transaction status = %d body=%s", ownResponse.Code, ownResponse.Body.String())
	}

	getOther := httptest.NewRequest(http.MethodGet,
		"/providers/provider-a/wagering/transactions/shared-external-id-b", nil)
	getOther.Header.Set("Authorization", "Bearer "+providerAToken)
	otherResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherResponse, getOther)
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-provider transaction status = %d, want 404; body=%s", otherResponse.Code, otherResponse.Body.String())
	}

	spoof := httptest.NewRequest(http.MethodPost, "/wagering/transactions",
		strings.NewReader(`{"providerId":"provider-b"}`))
	spoof.Header.Set("Authorization", "Bearer "+providerAToken)
	spoof.Header.Set("Idempotency-Key", "spoof")
	spoofResponse := httptest.NewRecorder()
	handler.ServeHTTP(spoofResponse, spoof)
	if spoofResponse.Code != http.StatusForbidden {
		t.Fatalf("spoofed provider id status = %d, want 403", spoofResponse.Code)
	}
}

func realHTTPIntegrationPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL integration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping PostgreSQL integration database: %v", err)
	}
	random := make([]byte, 10)
	if _, err := rand.Read(random); err != nil {
		admin.Close()
		t.Fatalf("generate isolated schema suffix: %v", err)
	}
	schema := "jungle_it_" + hex.EncodeToString(random)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatalf("parse test database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatalf("open isolated schema pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
		admin.Close()
	})
	if err := postgres.NewMigrationRunner(pool, migrationfiles.Files).Up(ctx); err != nil {
		t.Fatalf("apply migrations in isolated schema: %v", err)
	}
	return pool
}

func realClientToken(t *testing.T, ctx context.Context, issuer, clientID, secret string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(issuer, "/")+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("create OAuth token request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(clientID, secret)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("request OAuth token: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("OAuth token endpoint status = %d", response.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode OAuth token response: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("OAuth token response has no access token")
	}
	return result.AccessToken
}
