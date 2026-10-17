package port

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wallet"
)

type UnitOfWork interface {
	Within(context.Context, func(context.Context, Repositories) error) error
}

type Repositories interface {
	Wallets() WalletRepository
	Wagers() WagerRepository
	Ledger() LedgerRepository
	Idempotency() IdempotencyRepository
	Inbox() InboxRepository
	Outbox() OutboxRepository
	Reversals() ReversalRepository
}

type WalletRepository interface {
	Create(context.Context, *wallet.Wallet) error
	FindByID(context.Context, string) (*wallet.Wallet, error)
	FindByPlayerCurrency(context.Context, string, money.Currency) (*wallet.Wallet, error)
	LockByID(context.Context, string) (*wallet.Wallet, error)
	SaveBalance(context.Context, string, uint64, money.Money, uint64, time.Time) error
}

type WagerRepository interface {
	Create(context.Context, *wager.WagerTransaction) error
	FindByID(context.Context, string) (*wager.WagerTransaction, error)
	LockByID(context.Context, string) (*wager.WagerTransaction, error)
	FindByExternalID(context.Context, string, string) (*wager.WagerTransaction, error)
	FindByIdempotencyKey(context.Context, string, string) (*wager.WagerTransaction, error)
	UpdateState(context.Context, *wager.WagerTransaction) error
	FindPendingReferences(context.Context, int) ([]*wager.WagerTransaction, error)
	SchedulePendingReferenceCheck(context.Context, string, time.Time) error
}

type LedgerRepository interface {
	Append(context.Context, ledger.WalletLedgerEntry, uint64) error
	FindByTransactionID(context.Context, string, string) (ledger.WalletLedgerEntry, error)
	List(context.Context, string, *LedgerCursor, int) ([]ledger.WalletLedgerEntry, error)
	Reconcile(context.Context, string) (LedgerTotals, error)
}

type LedgerTotals struct {
	CreditsMinor int64
	DebitsMinor  int64
	EntryCount   int64
}

type IdempotencyRepository interface {
	Find(context.Context, string, string) (IdempotencyRecord, error)
}

type InboxRepository interface {
	Receive(context.Context, string, string, string, time.Time) (InboxReceipt, error)
	Complete(context.Context, string, string, string, time.Time) error
}

type OutboxRepository interface {
	Append(context.Context, OutboxEvent) error
	// ClaimBatch returns a per-claim LockOwner token; pass that token to the
	// corresponding MarkPublished or Reschedule call.
	ClaimBatch(context.Context, string, time.Time, time.Duration, int) ([]OutboxEvent, error)
	MarkPublished(context.Context, string, string, time.Time) error
	Reschedule(context.Context, string, string, string, time.Time) error
}

type ReversalRepository interface {
	RecordProcessed(context.Context, string, string, time.Time) error
	FindByReference(context.Context, string) (string, error)
}

type LedgerCursor struct {
	CreatedAt time.Time
	ID        string
}

type IdempotencyRecord struct {
	ProviderID    string
	Key           string
	PayloadHash   string
	TransactionID string
	CreatedAt     time.Time
}

type InboxState string

const (
	InboxReceived  InboxState = "RECEIVED"
	InboxCompleted InboxState = "COMPLETED"
)

type InboxReceipt struct {
	ConsumerName string
	MessageID    string
	PayloadHash  string
	State        InboxState
	ReceivedAt   time.Time
	CompletedAt  *time.Time
	Inserted     bool
}

type OutboxEvent struct {
	EventID       string
	AggregateID   string
	EventType     string
	SchemaVersion uint32
	CorrelationID string
	CausationID   string
	Payload       json.RawMessage
	OccurredAt    time.Time
	Attempts      int
	AvailableAt   time.Time
	LockOwner     string
	LockedUntil   *time.Time
	PublishedAt   *time.Time
	LastError     string
	CreatedAt     time.Time
}
