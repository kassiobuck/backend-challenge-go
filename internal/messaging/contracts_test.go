package messaging

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
)

func TestDecodeWagerCommand(t *testing.T) {
	valid := []byte(`{"providerId":"provider-a","externalTransactionId":"external-1","idempotencyKey":"key-1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f291-27dd-7d3f-8071-5f8685deef38","roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`)
	command, err := DecodeWagerCommand(valid)
	if err != nil {
		t.Fatalf("DecodeWagerCommand() error = %v", err)
	}
	if command.ProviderID != "provider-a" || command.Kind != wager.Bet || command.Money.String() != "1.00" {
		t.Fatalf("decoded command = %+v", command)
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "malformed JSON", raw: "{"},
		{name: "missing idempotency key", raw: `{"providerId":"p","externalTransactionId":"e","walletId":"w","playerId":"p","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`},
		{name: "unknown field", raw: `{"providerId":"p","externalTransactionId":"e","idempotencyKey":"k","walletId":"w","playerId":"p","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"1.00","currency":"BRL"},"trusted":true}`},
		{name: "trailing JSON", raw: `{"providerId":"p","externalTransactionId":"e","idempotencyKey":"k","walletId":"w","playerId":"p","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"1.00","currency":"BRL"}} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeWagerCommand([]byte(test.raw)); err == nil {
				t.Fatal("DecodeWagerCommand() error = nil, want validation error")
			}
		})
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: time.Second},
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 4, want: 8 * time.Second},
		{attempt: 20, want: 5 * time.Minute},
	} {
		if got := Backoff(test.attempt, time.Second, 5*time.Minute); got != test.want {
			t.Errorf("Backoff(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestTypedEventEnvelopeUsesVersionedWireContract(t *testing.T) {
	envelope := EventEnvelope[WagerTransactionRejectedData]{
		EventMetadata: NewEventMetadata("event-id", WagerTransactionRejectedType, "wallet-id", "correlation-id", "transaction-id", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)),
		Data: WagerTransactionRejectedData{
			TransactionID: "transaction-id", ProviderID: "provider-a",
			ExternalTransactionID: "external-1", Kind: wager.Bet,
			FailureCode: wager.FailureInsufficientFunds,
		},
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"eventId", "eventType", "schemaVersion", "aggregateId", "correlationId", "causationId", "occurredAt", "data"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("wire event lacks required field %q: %s", field, encoded)
		}
	}
	var data WagerTransactionRejectedData
	if err := json.Unmarshal(decoded["data"], &data); err != nil {
		t.Fatal(err)
	}
	if data.ProviderID != "provider-a" || data.FailureCode != wager.FailureInsufficientFunds {
		t.Fatalf("unexpected data after typed wire decode: %+v", data)
	}
}
