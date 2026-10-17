package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain/event"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
)

func Backoff(attempt int, base, maximum time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 || maximum <= 0 || base >= maximum {
		if maximum > 0 {
			return maximum
		}
		return base
	}
	delay := base
	for step := 1; step < attempt && delay < maximum; step++ {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

const (
	SchemaVersion uint32 = event.EventSchemaVersion

	WagerTransactionProcessedType        = string(event.WagerTransactionProcessedType)
	WagerTransactionRejectedType         = string(event.WagerTransactionRejectedType)
	WalletBalanceChangedType             = string(event.WalletBalanceChangedType)
	WagerTransactionPendingReferenceType = string(event.WagerTransactionPendingReferenceType)
)

type WagerCommand struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	IdempotencyKey                 string      `json:"idempotencyKey"`
	WalletID                       string      `json:"walletId"`
	PlayerID                       string      `json:"playerId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           wager.Kind  `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	CorrelationID                  string      `json:"correlationId,omitempty"`
}

func DecodeWagerCommand(raw []byte) (WagerCommand, error) {
	var command WagerCommand
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return WagerCommand{}, fmt.Errorf("decode wager command: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return WagerCommand{}, errors.New("wager command contains trailing JSON")
		}
		return WagerCommand{}, fmt.Errorf("decode trailing wager command JSON: %w", err)
	}
	if strings.TrimSpace(command.ProviderID) == "" ||
		strings.TrimSpace(command.ExternalTransactionID) == "" ||
		strings.TrimSpace(command.IdempotencyKey) == "" ||
		strings.TrimSpace(command.WalletID) == "" ||
		strings.TrimSpace(command.PlayerID) == "" ||
		strings.TrimSpace(command.RoundID) == "" ||
		strings.TrimSpace(command.GameID) == "" ||
		!command.Money.IsValid() {
		return WagerCommand{}, errors.New("wager command is missing required fields")
	}
	return command, nil
}

type EventMetadata struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	SchemaVersion uint32    `json:"schemaVersion"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId"`
	OccurredAt    time.Time `json:"occurredAt"`
}

type WagerTransactionProcessedData struct {
	TransactionID         string      `json:"transactionId"`
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	Kind                  wager.Kind  `json:"kind"`
	Amount                money.Money `json:"amount"`
	ResultingBalance      money.Money `json:"resultingBalance"`
}

type WagerTransactionRejectedData struct {
	TransactionID         string            `json:"transactionId"`
	ProviderID            string            `json:"providerId"`
	ExternalTransactionID string            `json:"externalTransactionId"`
	Kind                  wager.Kind        `json:"kind"`
	FailureCode           wager.FailureCode `json:"failureCode"`
}

type WalletBalanceChangedData struct {
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     ledger.Direction `json:"direction"`
	Amount        money.Money      `json:"amount"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion uint64           `json:"walletVersion"`
}

type WagerTransactionPendingReferenceData struct {
	TransactionID                  string      `json:"transactionId"`
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
	Kind                           wager.Kind  `json:"kind"`
	Amount                         money.Money `json:"amount"`
}

type EventEnvelope[T any] struct {
	EventMetadata
	Data T `json:"data"`
}

func NewEventMetadata(eventID, eventType, aggregateID, correlationID, causationID string, occurredAt time.Time) EventMetadata {
	return EventMetadata{
		EventID: eventID, EventType: eventType, SchemaVersion: SchemaVersion,
		AggregateID: aggregateID, CorrelationID: correlationID,
		CausationID: causationID, OccurredAt: occurredAt.UTC(),
	}
}
