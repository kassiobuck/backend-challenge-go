package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/identity"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wallet"
)

const maxRequestBody = 1 << 20

func registerBusinessRoutes(mux *http.ServeMux, service *application.FinancialService, verifier port.TokenVerifier, logger *slog.Logger) {
	mux.HandleFunc("POST /wallets", authenticate(verifier, "wallet:write", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			PlayerID       string      `json:"playerId"`
			InitialBalance money.Money `json:"initialBalance"`
		}
		if err := decodeJSON(w, r, &request); err != nil {
			writeInputError(w, err)
			return
		}
		opened, err := service.OpenWallet(r.Context(), application.OpenWalletCommand{
			PlayerID: request.PlayerID, InitialBalance: request.InitialBalance,
		})
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusCreated, walletResponse(opened.Wallet))
	}))
	mux.HandleFunc("GET /wallets/{walletID}", authenticate(verifier, "wallet:read", func(w http.ResponseWriter, r *http.Request) {
		entity, err := service.GetWallet(r.Context(), r.PathValue("walletID"))
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, walletResponse(entity))
	}))
	mux.HandleFunc("GET /wallets/{walletID}/ledger", authenticate(verifier, "ledger:read", func(w http.ResponseWriter, r *http.Request) {
		limit, err := parseLimit(r.URL.Query().Get("limit"), 50, 100)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", "limit must be an integer between 1 and 100")
			return
		}
		var cursor *port.LedgerCursor
		if encoded := r.URL.Query().Get("cursor"); encoded != "" {
			cursor, err = decodeLedgerCursor(encoded, r.PathValue("walletID"))
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_input", "cursor is invalid for this wallet")
				return
			}
		}
		page, err := service.ListWalletLedger(r.Context(), r.PathValue("walletID"), cursor, limit)
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		response := ledgerPageResponse{Entries: make([]ledgerEntryResponse, 0, len(page.Entries))}
		for _, entry := range page.Entries {
			response.Entries = append(response.Entries, ledgerResponse(entry))
		}
		if page.HasMore && len(page.Entries) > 0 {
			last := page.Entries[len(page.Entries)-1]
			response.NextCursor = encodeLedgerCursor(r.PathValue("walletID"), port.LedgerCursor{
				CreatedAt: last.CreatedAt(), ID: last.ID(),
			})
		}
		writeJSON(w, http.StatusOK, response)
	}))
	mux.HandleFunc("POST /wallets/{walletID}/reconciliation", authenticate(verifier, "reconciliation:write", func(w http.ResponseWriter, r *http.Request) {
		if err := rejectUnexpectedBody(r); err != nil {
			writeInputError(w, err)
			return
		}
		reconciliation, err := service.ReconcileWallet(r.Context(), r.PathValue("walletID"))
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		stored := reconciliation.Wallet.Balance()
		difference, err := stored.Sub(reconciliation.LedgerBalance)
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		if !reconciliation.Reconciled {
			logger.Error("wallet ledger reconciliation mismatch",
				"wallet_id", reconciliation.Wallet.ID(),
				"stored_balance", stored.String(),
				"calculated_balance", reconciliation.LedgerBalance.String(),
				"difference", difference.String(),
			)
		}
		writeJSON(w, http.StatusOK, struct {
			WalletID          string      `json:"walletId"`
			StoredBalance     money.Money `json:"storedBalance"`
			CalculatedBalance money.Money `json:"calculatedBalance"`
			Difference        money.Money `json:"difference"`
			Consistent        bool        `json:"consistent"`
			CheckedEntries    int64       `json:"checkedEntries"`
		}{
			WalletID: reconciliation.Wallet.ID(), StoredBalance: stored,
			CalculatedBalance: reconciliation.LedgerBalance, Difference: difference,
			Consistent: reconciliation.Reconciled, CheckedEntries: reconciliation.EntryCount,
		})
	}))
	mux.HandleFunc("POST /wagering/transactions", authenticate(verifier, "wager:write", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		var request struct {
			ProviderID                     string      `json:"providerId"`
			ExternalTransactionID          string      `json:"externalTransactionId"`
			PlayerID                       string      `json:"playerId"`
			WalletID                       string      `json:"walletId"`
			RoundID                        string      `json:"roundId"`
			GameID                         string      `json:"gameId"`
			Kind                           wager.Kind  `json:"kind"`
			Money                          money.Money `json:"money"`
			ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
		}
		if err := decodeJSON(w, r, &request); err != nil {
			writeInputError(w, err)
			return
		}
		if err := providerIdentityMatchesBody(request.ProviderID, principal); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", "providerId must match the authenticated provider")
			return
		}
		idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if idempotencyKey == "" {
			writeError(w, http.StatusBadRequest, "invalid_input", "Idempotency-Key header is required")
			return
		}
		result, err := service.ProcessWager(r.Context(), application.ProcessWagerCommand{
			ProviderID:            principal.ProviderID,
			ExternalTransactionID: request.ExternalTransactionID,
			IdempotencyKey:        idempotencyKey, WalletID: request.WalletID,
			PlayerID: request.PlayerID, RoundID: request.RoundID, GameID: request.GameID,
			Kind: request.Kind, Amount: request.Money,
			ReferenceExternalTransactionID: request.ReferenceExternalTransactionID,
			CorrelationID:                  r.Header.Get("X-Correlation-ID"),
		})
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		writeWagerResult(w, result)
	}))
	mux.HandleFunc("GET /wagering/transactions/{transactionID}", authenticate(verifier, "wager:internal:read", func(w http.ResponseWriter, r *http.Request) {
		transaction, err := service.GetWager(r.Context(), r.PathValue("transactionID"))
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, transactionResponse(transaction, false))
	}))
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", authenticate(verifier, "wager:read", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		if !requireProviderPathIdentity(w, r, principal) {
			return
		}
		transaction, err := service.GetProviderWager(r.Context(), principal.ProviderID, r.PathValue("externalTransactionId"))
		if err != nil {
			writeUseCaseError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, transactionResponse(transaction, false))
	}))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return fmt.Errorf("decode trailing request JSON: %w", err)
	}
	return nil
}

func rejectUnexpectedBody(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	var body [1]byte
	n, err := r.Body.Read(body[:])
	if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return errors.New("request body must be empty")
	}
	return nil
}

func writeInputError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, "invalid_input", "request body or parameters are invalid")
}

func writeUseCaseError(w http.ResponseWriter, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, port.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, application.ErrIdempotencyConflict),
		errors.Is(err, application.ErrExternalTransactionConflict),
		errors.Is(err, application.ErrWalletAlreadyExists),
		errors.Is(err, port.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "request conflicts with an existing resource")
	case errors.Is(err, application.ErrWalletOwnership):
		writeError(w, http.StatusForbidden, "forbidden", "the authenticated provider cannot access this wallet")
	case errors.Is(err, application.ErrRetryableConflict):
		writeError(w, http.StatusServiceUnavailable, "transient_failure", "the operation could not be completed; retry safely")
	case errors.Is(err, domain.ErrInvalidID), errors.Is(err, domain.ErrInvalidWallet),
		errors.Is(err, domain.ErrInvalidMoney), errors.Is(err, domain.ErrInvalidCurrency),
		errors.Is(err, domain.ErrUnsupportedCurrency), errors.Is(err, domain.ErrInvalidWager),
		errors.Is(err, domain.ErrInvalidWagerKind), errors.Is(err, domain.ErrReferenceRequired),
		errors.Is(err, domain.ErrCurrencyMismatch), errors.Is(err, domain.ErrInvalidTimestamp):
		writeError(w, http.StatusBadRequest, "invalid_input", "request body or parameters are invalid")
	default:
		logger.Error("HTTP business operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

func writeWagerResult(w http.ResponseWriter, result *application.WagerResult) {
	status := http.StatusOK
	switch result.Transaction.Status() {
	case wager.Pending, wager.PendingReference:
		status = http.StatusAccepted
	case wager.Rejected:
		status = http.StatusUnprocessableEntity
	case wager.Failed:
		status = http.StatusServiceUnavailable
	default:
		if !result.Replay {
			status = http.StatusCreated
		}
	}
	writeJSON(w, status, transactionResponse(result.Transaction, result.Replay))
}

type transactionDTO struct {
	TransactionID         string            `json:"transactionId"`
	ProviderID            string            `json:"providerId,omitempty"`
	ExternalTransactionID string            `json:"externalTransactionId,omitempty"`
	WalletID              string            `json:"walletId"`
	PlayerID              string            `json:"playerId"`
	RoundID               string            `json:"roundId,omitempty"`
	GameID                string            `json:"gameId,omitempty"`
	Kind                  wager.Kind        `json:"kind"`
	Amount                money.Money       `json:"money"`
	Status                wager.Status      `json:"status"`
	FailureCode           wager.FailureCode `json:"failureCode,omitempty"`
	Balance               *money.Money      `json:"balance,omitempty"`
	IdempotentReplay      bool              `json:"idempotentReplay"`
	CreatedAt             time.Time         `json:"createdAt"`
	UpdatedAt             time.Time         `json:"updatedAt"`
}

func transactionResponse(transaction *wager.WagerTransaction, replay bool) transactionDTO {
	snapshot := transaction.Snapshot()
	response := transactionDTO{
		TransactionID: snapshot.ID, ProviderID: snapshot.ProviderID,
		ExternalTransactionID: snapshot.ExternalTransactionID,
		WalletID:              snapshot.WalletID, PlayerID: snapshot.PlayerID,
		RoundID: snapshot.RoundID, GameID: snapshot.GameID,
		Kind: snapshot.Kind, Amount: snapshot.Amount, Status: snapshot.Status,
		FailureCode: snapshot.FailureCode, IdempotentReplay: replay,
		CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt,
	}
	if snapshot.HasResultBalance {
		balance := snapshot.ResultBalance
		response.Balance = &balance
	}
	return response
}

func walletResponse(entity *wallet.Wallet) any {
	return struct {
		ID       string      `json:"id"`
		PlayerID string      `json:"playerId"`
		Balance  money.Money `json:"balance"`
		Version  uint64      `json:"version"`
	}{entity.ID(), entity.PlayerID(), entity.Balance(), entity.Version()}
}

type ledgerEntryResponse struct {
	ID            string           `json:"id"`
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     ledger.Direction `json:"direction"`
	Amount        money.Money      `json:"amount"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	CreatedAt     time.Time        `json:"createdAt"`
}

type ledgerPageResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func ledgerResponse(entry ledger.WalletLedgerEntry) ledgerEntryResponse {
	return ledgerEntryResponse{
		ID: entry.ID(), WalletID: entry.WalletID(), TransactionID: entry.TransactionID(),
		Direction: entry.Direction(), Amount: entry.Amount(),
		BalanceBefore: entry.BalanceBefore(), BalanceAfter: entry.BalanceAfter(),
		CreatedAt: entry.CreatedAt(),
	}
}

type encodedLedgerCursor struct {
	Version   int       `json:"v"`
	WalletID  string    `json:"w"`
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"i"`
}

func encodeLedgerCursor(walletID string, cursor port.LedgerCursor) string {
	raw, _ := json.Marshal(encodedLedgerCursor{Version: 1, WalletID: walletID, CreatedAt: cursor.CreatedAt, ID: cursor.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeLedgerCursor(encoded, walletID string) (*port.LedgerCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) > 512 {
		return nil, errors.New("invalid cursor encoding")
	}
	var cursor encodedLedgerCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return nil, fmt.Errorf("decode cursor: %w", err)
	}
	if cursor.Version != 1 || cursor.WalletID != walletID || cursor.CreatedAt.IsZero() ||
		identity.Validate(cursor.ID) != nil {
		return nil, errors.New("cursor does not match wallet or schema")
	}
	return &port.LedgerCursor{CreatedAt: cursor.CreatedAt.UTC(), ID: cursor.ID}, nil
}

func parseLimit(raw string, defaultValue, maximum int) (int, error) {
	if raw == "" {
		return defaultValue, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maximum {
		return 0, errors.New("limit is out of range")
	}
	return limit, nil
}
