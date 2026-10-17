package event

import (
	"errors"
	"testing"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
)

const (
	eventID     = "0192f291-27dd-7d3f-8071-5f8685deef37"
	eventWallet = "0192f292-27dd-7d3f-8071-5f8685deef37"
	eventPlayer = "0192f293-27dd-7d3f-8071-5f8685deef37"
	eventTxn    = "0192f294-27dd-7d3f-8071-5f8685deef37"
)

func TestWagerTransactionProcessedEvent(t *testing.T) {
	now := time.Now().In(time.FixedZone("test", 3600))
	transaction := newProcessedWager(t, now)
	event, err := NewWagerTransactionProcessed(metadata(now), transaction)
	if err != nil {
		t.Fatalf("NewWagerTransactionProcessed() error = %v", err)
	}
	if event.EventType() != WagerTransactionProcessedType || event.Version() != EventSchemaVersion {
		t.Fatalf("event type/version = %s/%d", event.EventType(), event.Version())
	}
	if event.OccurredAt().Location() != time.UTC {
		t.Fatalf("occurredAt location = %v, want UTC", event.OccurredAt().Location())
	}
	if event.Data().ResultingBalance().String() != "90.00" {
		t.Fatalf("resulting balance = %s, want 90.00", event.Data().ResultingBalance())
	}
}

func TestWagerTransactionRejectedEvent(t *testing.T) {
	now := time.Now().UTC()
	transaction := newPendingWager(t, now)
	if err := transaction.Reject(wager.FailureInsufficientFunds, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	event, err := NewWagerTransactionRejected(metadata(now), transaction)
	if err != nil {
		t.Fatalf("NewWagerTransactionRejected() error = %v", err)
	}
	if event.Data().FailureCode() != wager.FailureInsufficientFunds || event.EventType() != WagerTransactionRejectedType {
		t.Fatalf("rejection event data/type = %s/%s", event.Data().FailureCode(), event.EventType())
	}
}

func TestWalletBalanceChangedEvent(t *testing.T) {
	entry, err := ledger.NewEntry(
		eventID, eventWallet, eventTxn, ledger.Debit,
		mustMoney(t, "10.00"), mustMoney(t, "100.00"), mustMoney(t, "90.00"), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	meta := metadata(time.Now())
	meta.CausationID = eventTxn
	event, err := NewWalletBalanceChanged(meta, entry, 2)
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged() error = %v", err)
	}
	if event.Data().WalletID() != eventWallet ||
		event.Data().Direction() != ledger.Debit ||
		event.Data().WalletVersion() != 2 ||
		event.Data().BalanceAfter().String() != "90.00" {
		t.Fatalf("balance event data does not match ledger entry")
	}
}

func TestPendingReferenceEvent(t *testing.T) {
	now := time.Now().UTC()
	params := wager.ExternalParams{
		ID: eventTxn, ProviderID: "provider-a", ExternalTransactionID: "refund-1",
		IdempotencyKey: "provider-a:refund-1", PayloadHash: stringHash(),
		WalletID: eventWallet, PlayerID: eventPlayer, RoundID: "round-1",
		GameID: "game-1", Kind: wager.Refund, Amount: mustMoney(t, "10.00"),
		ReferenceExternalTransactionID: "bet-1", CreatedAt: now,
	}
	transaction, err := wager.NewExternal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WaitForReference(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	event, err := NewWagerTransactionPendingReference(metadata(now), transaction)
	if err != nil {
		t.Fatalf("NewWagerTransactionPendingReference() error = %v", err)
	}
	if event.Data().ReferenceExternalTransactionID() != "bet-1" {
		t.Fatalf("reference ID = %q, want bet-1", event.Data().ReferenceExternalTransactionID())
	}
}

func TestEventConstructorsRejectInvalidInputs(t *testing.T) {
	now := time.Now().UTC()
	metadataCases := []struct {
		name   string
		change func(*Metadata)
	}{
		{name: "bad event id", change: func(m *Metadata) { m.EventID = "bad" }},
		{name: "bad aggregate id", change: func(m *Metadata) { m.AggregateID = "bad" }},
		{name: "missing correlation", change: func(m *Metadata) { m.CorrelationID = "" }},
		{name: "bad causation", change: func(m *Metadata) { m.CausationID = "bad" }},
		{name: "missing timestamp", change: func(m *Metadata) { m.OccurredAt = time.Time{} }},
	}
	for _, test := range metadataCases {
		t.Run(test.name, func(t *testing.T) {
			meta := metadata(now)
			test.change(&meta)
			_, err := NewWagerTransactionProcessed(meta, newProcessedWager(t, now))
			if !errors.Is(err, domain.ErrInvalidEvent) {
				t.Fatalf("constructor error = %v, want ErrInvalidEvent", err)
			}
		})
	}

	t.Run("processed event rejects pending transaction", func(t *testing.T) {
		_, err := NewWagerTransactionProcessed(metadata(now), newPendingWager(t, now))
		if !errors.Is(err, domain.ErrInvalidEvent) {
			t.Fatalf("constructor error = %v, want ErrInvalidEvent", err)
		}
	})

	t.Run("balance event requires causation to match transaction", func(t *testing.T) {
		entry, err := ledger.NewEntry(
			eventID, eventWallet, eventTxn, ledger.Credit,
			mustMoney(t, "1.00"), mustMoney(t, "1.00"), mustMoney(t, "2.00"), now,
		)
		if err != nil {
			t.Fatal(err)
		}
		meta := metadata(now)
		meta.CausationID = eventPlayer
		if _, err := NewWalletBalanceChanged(meta, entry, 2); !errors.Is(err, domain.ErrInvalidEvent) {
			t.Fatalf("constructor error = %v, want ErrInvalidEvent", err)
		}
	})
}

func TestOpeningProcessedEventAllowsInternalTransaction(t *testing.T) {
	now := time.Now().UTC()
	opening, err := wager.NewOpening(wager.OpeningParams{
		ID: eventTxn, WalletID: eventWallet, PlayerID: eventPlayer,
		Amount: mustMoney(t, "100.00"), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := NewWagerTransactionProcessed(metadata(now), opening)
	if err != nil {
		t.Fatalf("opening processed event error = %v", err)
	}
	if event.Data().Kind() != wager.Opening || event.Data().ProviderID() != "" {
		t.Fatalf("opening event kind/provider = %s/%q", event.Data().Kind(), event.Data().ProviderID())
	}
}

func newProcessedWager(t *testing.T, now time.Time) *wager.WagerTransaction {
	t.Helper()
	transaction := newPendingWager(t, now)
	if err := transaction.Process(mustMoney(t, "100.00"), mustMoney(t, "90.00"), nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return transaction
}

func newPendingWager(t *testing.T, now time.Time) *wager.WagerTransaction {
	t.Helper()
	transaction, err := wager.NewExternal(wager.ExternalParams{
		ID: eventTxn, ProviderID: "provider-a", ExternalTransactionID: "bet-1",
		IdempotencyKey: "provider-a:bet-1", PayloadHash: stringHash(),
		WalletID: eventWallet, PlayerID: eventPlayer, RoundID: "round-1",
		GameID: "game-1", Kind: wager.Bet, Amount: mustMoney(t, "10.00"), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func metadata(now time.Time) Metadata {
	return Metadata{
		EventID: eventID, AggregateID: eventWallet, CorrelationID: "correlation-1",
		OccurredAt: now,
	}
}

func stringHash() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	value, err := money.ParseAmount(amount, money.BRL)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", amount, err)
	}
	return value
}
