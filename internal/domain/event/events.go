package event

import (
	"strings"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/identity"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
)

type Type string

const (
	WagerTransactionProcessedType        Type   = "WagerTransactionProcessed"
	WagerTransactionRejectedType         Type   = "WagerTransactionRejected"
	WalletBalanceChangedType             Type   = "WalletBalanceChanged"
	WagerTransactionPendingReferenceType Type   = "WagerTransactionPendingReference"
	EventSchemaVersion                   uint32 = 1
)

type Metadata struct {
	EventID       string
	AggregateID   string
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

type Envelope[T any] struct {
	eventID       string
	eventType     Type
	aggregateID   string
	correlationID string
	causationID   string
	occurredAt    time.Time
	version       uint32
	data          T
}

type WagerTransactionProcessedData struct {
	transactionID         string
	providerID            string
	externalTransactionID string
	kind                  wager.Kind
	amount                money.Money
	resultingBalance      money.Money
}

type WagerTransactionRejectedData struct {
	transactionID         string
	providerID            string
	externalTransactionID string
	kind                  wager.Kind
	failureCode           wager.FailureCode
}

type WalletBalanceChangedData struct {
	walletID      string
	transactionID string
	direction     ledger.Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	walletVersion uint64
}

type WagerTransactionPendingReferenceData struct {
	transactionID                  string
	providerID                     string
	externalTransactionID          string
	referenceExternalTransactionID string
	kind                           wager.Kind
	amount                         money.Money
}

func NewWagerTransactionProcessed(meta Metadata, transaction *wager.WagerTransaction) (Envelope[WagerTransactionProcessedData], error) {
	if err := validateMetadata(meta); err != nil {
		return Envelope[WagerTransactionProcessedData]{}, err
	}
	if transaction == nil || transaction.Status() != wager.Processed {
		return Envelope[WagerTransactionProcessedData]{}, domain.ErrInvalidEvent
	}
	balance, ok := transaction.ResultBalance()
	if !ok || !balance.IsValid() || balance.IsNegative() {
		return Envelope[WagerTransactionProcessedData]{}, domain.ErrInvalidEvent
	}
	if meta.AggregateID != transaction.WalletID() {
		return Envelope[WagerTransactionProcessedData]{}, domain.ErrInvalidEvent
	}
	return Envelope[WagerTransactionProcessedData]{
		eventID: meta.EventID, eventType: WagerTransactionProcessedType,
		aggregateID: meta.AggregateID, correlationID: meta.CorrelationID,
		causationID: meta.CausationID, occurredAt: meta.OccurredAt.UTC(),
		version: EventSchemaVersion,
		data: WagerTransactionProcessedData{
			transactionID: transaction.ID(), providerID: transaction.ProviderID(),
			externalTransactionID: transaction.ExternalTransactionID(),
			kind:                  transaction.Kind(), amount: transaction.Amount(), resultingBalance: balance,
		},
	}, nil
}

func NewWagerTransactionRejected(meta Metadata, transaction *wager.WagerTransaction) (Envelope[WagerTransactionRejectedData], error) {
	if err := validateMetadata(meta); err != nil {
		return Envelope[WagerTransactionRejectedData]{}, err
	}
	if transaction == nil || transaction.Status() != wager.Rejected ||
		transaction.FailureCode() == "" || meta.AggregateID != transaction.WalletID() {
		return Envelope[WagerTransactionRejectedData]{}, domain.ErrInvalidEvent
	}
	return Envelope[WagerTransactionRejectedData]{
		eventID: meta.EventID, eventType: WagerTransactionRejectedType,
		aggregateID: meta.AggregateID, correlationID: meta.CorrelationID,
		causationID: meta.CausationID, occurredAt: meta.OccurredAt.UTC(),
		version: EventSchemaVersion,
		data: WagerTransactionRejectedData{
			transactionID: transaction.ID(), providerID: transaction.ProviderID(),
			externalTransactionID: transaction.ExternalTransactionID(),
			kind:                  transaction.Kind(), failureCode: transaction.FailureCode(),
		},
	}, nil
}

func NewWalletBalanceChanged(meta Metadata, entry ledger.WalletLedgerEntry, walletVersion uint64) (Envelope[WalletBalanceChangedData], error) {
	if err := validateMetadata(meta); err != nil {
		return Envelope[WalletBalanceChangedData]{}, err
	}
	if meta.AggregateID != entry.WalletID() || meta.CausationID != entry.TransactionID() || walletVersion == 0 {
		return Envelope[WalletBalanceChangedData]{}, domain.ErrInvalidEvent
	}
	return Envelope[WalletBalanceChangedData]{
		eventID: meta.EventID, eventType: WalletBalanceChangedType,
		aggregateID: meta.AggregateID, correlationID: meta.CorrelationID,
		causationID: meta.CausationID, occurredAt: meta.OccurredAt.UTC(),
		version: EventSchemaVersion,
		data: WalletBalanceChangedData{
			walletID: entry.WalletID(), transactionID: entry.TransactionID(),
			direction: entry.Direction(), amount: entry.Amount(),
			balanceBefore: entry.BalanceBefore(), balanceAfter: entry.BalanceAfter(),
			walletVersion: walletVersion,
		},
	}, nil
}

func NewWagerTransactionPendingReference(meta Metadata, transaction *wager.WagerTransaction) (Envelope[WagerTransactionPendingReferenceData], error) {
	if err := validateMetadata(meta); err != nil {
		return Envelope[WagerTransactionPendingReferenceData]{}, err
	}
	if transaction == nil || transaction.Status() != wager.PendingReference ||
		transaction.ReferenceExternalTransactionID() == "" ||
		meta.AggregateID != transaction.WalletID() {
		return Envelope[WagerTransactionPendingReferenceData]{}, domain.ErrInvalidEvent
	}
	return Envelope[WagerTransactionPendingReferenceData]{
		eventID: meta.EventID, eventType: WagerTransactionPendingReferenceType,
		aggregateID: meta.AggregateID, correlationID: meta.CorrelationID,
		causationID: meta.CausationID, occurredAt: meta.OccurredAt.UTC(),
		version: EventSchemaVersion,
		data: WagerTransactionPendingReferenceData{
			transactionID: transaction.ID(), providerID: transaction.ProviderID(),
			externalTransactionID:          transaction.ExternalTransactionID(),
			referenceExternalTransactionID: transaction.ReferenceExternalTransactionID(),
			kind:                           transaction.Kind(), amount: transaction.Amount(),
		},
	}, nil
}

func validateMetadata(meta Metadata) error {
	if identity.Validate(meta.EventID) != nil || identity.Validate(meta.AggregateID) != nil ||
		strings.TrimSpace(meta.CorrelationID) == "" || meta.OccurredAt.IsZero() {
		return domain.ErrInvalidEvent
	}
	if meta.CausationID != "" && identity.Validate(meta.CausationID) != nil {
		return domain.ErrInvalidEvent
	}
	return nil
}

func (e Envelope[T]) EventID() string       { return e.eventID }
func (e Envelope[T]) EventType() Type       { return e.eventType }
func (e Envelope[T]) AggregateID() string   { return e.aggregateID }
func (e Envelope[T]) CorrelationID() string { return e.correlationID }
func (e Envelope[T]) CausationID() string   { return e.causationID }
func (e Envelope[T]) OccurredAt() time.Time { return e.occurredAt }
func (e Envelope[T]) Version() uint32       { return e.version }
func (e Envelope[T]) Data() T               { return e.data }

func (d WagerTransactionProcessedData) TransactionID() string         { return d.transactionID }
func (d WagerTransactionProcessedData) ProviderID() string            { return d.providerID }
func (d WagerTransactionProcessedData) ExternalTransactionID() string { return d.externalTransactionID }
func (d WagerTransactionProcessedData) Kind() wager.Kind              { return d.kind }
func (d WagerTransactionProcessedData) Amount() money.Money           { return d.amount }
func (d WagerTransactionProcessedData) ResultingBalance() money.Money { return d.resultingBalance }

func (d WagerTransactionRejectedData) TransactionID() string          { return d.transactionID }
func (d WagerTransactionRejectedData) ProviderID() string             { return d.providerID }
func (d WagerTransactionRejectedData) ExternalTransactionID() string  { return d.externalTransactionID }
func (d WagerTransactionRejectedData) Kind() wager.Kind               { return d.kind }
func (d WagerTransactionRejectedData) FailureCode() wager.FailureCode { return d.failureCode }

func (d WalletBalanceChangedData) WalletID() string            { return d.walletID }
func (d WalletBalanceChangedData) TransactionID() string       { return d.transactionID }
func (d WalletBalanceChangedData) Direction() ledger.Direction { return d.direction }
func (d WalletBalanceChangedData) Amount() money.Money         { return d.amount }
func (d WalletBalanceChangedData) BalanceBefore() money.Money  { return d.balanceBefore }
func (d WalletBalanceChangedData) BalanceAfter() money.Money   { return d.balanceAfter }
func (d WalletBalanceChangedData) WalletVersion() uint64       { return d.walletVersion }

func (d WagerTransactionPendingReferenceData) TransactionID() string { return d.transactionID }
func (d WagerTransactionPendingReferenceData) ProviderID() string    { return d.providerID }
func (d WagerTransactionPendingReferenceData) ExternalTransactionID() string {
	return d.externalTransactionID
}
func (d WagerTransactionPendingReferenceData) ReferenceExternalTransactionID() string {
	return d.referenceExternalTransactionID
}
func (d WagerTransactionPendingReferenceData) Kind() wager.Kind    { return d.kind }
func (d WagerTransactionPendingReferenceData) Amount() money.Money { return d.amount }
