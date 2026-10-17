package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/domain"
)

func TestLedgerCursorRoundTripAndValidation(t *testing.T) {
	const walletID = "0192f291-27dd-7d3f-8071-5f8685deef37"
	const entryID = "0192f291-27dd-7d3f-8071-5f8685deef38"
	createdAt := time.Date(2025, time.January, 2, 3, 4, 5, 6, time.UTC)
	encoded := encodeLedgerCursor(walletID, port.LedgerCursor{CreatedAt: createdAt, ID: entryID})

	cursor, err := decodeLedgerCursor(encoded, walletID)
	if err != nil {
		t.Fatalf("decodeLedgerCursor() error = %v", err)
	}
	if cursor.ID != entryID || !cursor.CreatedAt.Equal(createdAt) {
		t.Fatalf("decoded cursor = %+v, want ID %q and time %s", cursor, entryID, createdAt)
	}

	for _, test := range []struct {
		name     string
		encoded  string
		walletID string
	}{
		{name: "malformed encoding", encoded: "%%%"},
		{name: "cursor bound to another wallet", encoded: encoded, walletID: "0192f291-27dd-7d3f-8071-5f8685deef39"},
		{name: "unsupported version", encoded: base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"w":"` + walletID + `","t":"2025-01-02T03:04:05Z","i":"` + entryID + `"}`)), walletID: walletID},
		{name: "invalid entry ID", encoded: base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"w":"` + walletID + `","t":"2025-01-02T03:04:05Z","i":"invalid"}`)), walletID: walletID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeLedgerCursor(test.encoded, test.walletID); err == nil {
				t.Fatal("decodeLedgerCursor() error = nil, want invalid cursor error")
			}
		})
	}
}

func TestParseLimit(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
		ok   bool
	}{
		{raw: "", want: 50, ok: true},
		{raw: "1", want: 1, ok: true},
		{raw: "100", want: 100, ok: true},
		{raw: "0"},
		{raw: "101"},
		{raw: "not-a-number"},
	} {
		got, err := parseLimit(test.raw, 50, 100)
		if test.ok {
			if err != nil || got != test.want {
				t.Errorf("parseLimit(%q) = %d, %v; want %d, nil", test.raw, got, err, test.want)
			}
		} else if err == nil {
			t.Errorf("parseLimit(%q) = %d, nil; want an error", test.raw, got)
		}
	}
}

func TestUseCaseErrorsMapToHTTPContract(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		statusCode int
		code       string
	}{
		{name: "missing resource", err: port.ErrNotFound, statusCode: http.StatusNotFound, code: "not_found"},
		{name: "idempotency conflict", err: application.ErrIdempotencyConflict, statusCode: http.StatusConflict, code: "conflict"},
		{name: "invalid money", err: domain.ErrInvalidMoney, statusCode: http.StatusBadRequest, code: "invalid_input"},
		{name: "wrong wallet owner", err: application.ErrWalletOwnership, statusCode: http.StatusForbidden, code: "forbidden"},
		{name: "retryable conflict", err: application.ErrRetryableConflict, statusCode: http.StatusServiceUnavailable, code: "transient_failure"},
		{name: "unexpected failure", err: errors.New("internal details"), statusCode: http.StatusInternalServerError, code: "internal_error"},
	}
	logger := slog.New(slog.NewTextHandler(httptest.NewRecorder(), nil))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeUseCaseError(response, logger, test.err)
			if response.Code != test.statusCode {
				t.Fatalf("status = %d, want %d", response.Code, test.statusCode)
			}
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Code != test.code || body.Message == "" {
				t.Fatalf("error response = %+v, want code %q and non-empty message", body, test.code)
			}
		})
	}
}
