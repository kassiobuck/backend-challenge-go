package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wallet"
	migrationfiles "github.com/kassiobuck/backend-challenge-go/migrations"
)

func TestPostgresMigrationsUpDownAndIdempotency(t *testing.T) {
	pool := integrationPool(t)
	runner := NewMigrationRunner(pool, migrationfiles.Files)
	ctx := context.Background()

	if err := runner.Up(ctx); err != nil {
		t.Fatalf("first Up(): %v", err)
	}
	if err := runner.Up(ctx); err != nil {
		t.Fatalf("second Up(): %v", err)
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 3 {
		t.Fatalf("applied migration count = %d, want 3", applied)
	}

	if err := runner.Down(ctx, 2); err != nil {
		t.Fatalf("Down(reference retry migrations): %v", err)
	}
	var scheduleColumnExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'wager_transactions'
				AND column_name = 'reference_next_attempt_at'
		)`).Scan(&scheduleColumnExists); err != nil {
		t.Fatal(err)
	}
	if scheduleColumnExists {
		t.Fatal("reference retry schedule column remains after reverting its migration")
	}
	if err := runner.Down(ctx, 1); err != nil {
		t.Fatalf("Down(core migration): %v", err)
	}
	var tableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('wallets') IS NOT NULL`).Scan(&tableExists); err != nil {
		t.Fatal(err)
	}
	if tableExists {
		t.Fatal("wallets table still exists after down migration")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("migration count after down = %d, want 0", applied)
	}
	if err := runner.Up(ctx); err != nil {
		t.Fatalf("Up() after Down(): %v", err)
	}
}

func TestPostgresConcurrentMigrationStartupSerializesInitialSchemaCreation(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	runner := NewMigrationRunner(pool, migrationfiles.Files)
	if err := runner.Down(ctx, 3); err != nil {
		t.Fatalf("revert migrations in isolated schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("remove isolated migration table: %v", err)
	}

	const instances = 3
	start := make(chan struct{})
	errorsByInstance := make([]error, instances)
	var wait sync.WaitGroup
	wait.Add(instances)
	for index := 0; index < instances; index++ {
		go func(index int) {
			defer wait.Done()
			<-start
			errorsByInstance[index] = NewMigrationRunner(pool, migrationfiles.Files).Up(ctx)
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByInstance {
		if err != nil {
			t.Errorf("instance %d migration startup: %v", index, err)
		}
	}

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 3 {
		t.Fatalf("applied migrations = %d, want each of 3 migrations exactly once", applied)
	}
}

func TestPostgresConstraintsAndAppendOnlyLedger(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx := context.Background()
	walletEntity, opening, openingEntry, transaction, entry := financialFixture(t)
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wallets().Create(ctx, walletEntity); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, opening); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, openingEntry, 1); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, transaction); err != nil {
			return err
		}
		if err := repos.Wallets().SaveBalance(ctx, walletEntity.ID(), walletEntity.Version(), entry.BalanceAfter(), 2, entry.CreatedAt()); err != nil {
			return err
		}
		return repos.Ledger().Append(ctx, entry, 2)
	}); err != nil {
		t.Fatalf("persist financial fixture: %v", err)
	}

	duplicateWallet, err := wallet.Open(testWalletID2, testPlayerID, mustTestMoney(t, "1.00", money.BRL), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Wallets().Create(ctx, duplicateWallet)
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate wallet error = %v, want ErrConflict", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE wallets SET balance_minor = -1 WHERE id = $1`, testWalletID); !isConstraintError(err) {
		t.Fatalf("negative balance error = %v, want a database constraint error", err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Ledger().Append(ctx, entry, 2)
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate ledger entry error = %v, want ErrConflict", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE id = $1`, testLedgerID); err == nil {
		t.Fatal("ledger UPDATE succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM wallet_ledger_entries WHERE id = $1`, testLedgerID); err == nil {
		t.Fatal("ledger DELETE succeeded")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount_minor, currency,
			 balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES ($1, $2, $3, 'CREDIT', 1, 'USD', 0, 1, 2, now())`,
		testLedgerID2, testWalletID, testTransactionID); !isConstraintError(err) {
		t.Fatalf("ledger currency mismatch error = %v, want a database constraint error", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount_minor, currency,
			 balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES ($1, $2, $3, 'CREDIT', 1, 'BRL', 0, 1, 2, now())`,
		testLedgerID2, testWalletID, testThirdTransactionID); !isConstraintError(err) {
		t.Fatalf("ledger transaction foreign key error = %v, want a database constraint error", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE wallets SET balance_minor = 89 WHERE id = $1`, testWalletID); !isConstraintError(err) {
		t.Fatalf("wallet write without version increment error = %v, want a database constraint error", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM wallets WHERE id = $1`, testWalletID); err == nil {
		t.Fatal("wallet DELETE succeeded")
	}
	if _, err := pool.Exec(ctx, `UPDATE idempotency_records SET payload_hash = $1 WHERE provider_id = 'provider-a'`, hashB); err == nil {
		t.Fatal("idempotency record UPDATE succeeded")
	}
	if _, err := pool.Exec(ctx, `UPDATE wager_transactions SET round_id = 'rewritten' WHERE id = $1`, testTransactionID); err == nil {
		t.Fatal("wager request identity UPDATE succeeded")
	}
}

func TestPostgresNonZeroWalletRequiresMatchingOpeningLedger(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	initial := mustTestMoney(t, "25.00", money.BRL)
	entity, err := wallet.Open(testWalletID, testPlayerID, initial, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Wallets().Create(ctx, entity)
	}); !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("non-zero wallet without ledger error = %v, want ErrConstraintViolation", err)
	}

	opening, err := wager.NewOpening(wager.OpeningParams{
		ID: testTransactionID, WalletID: testWalletID, PlayerID: testPlayerID,
		Amount: initial, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ledger.NewEntry(
		testLedgerID, testWalletID, testTransactionID, ledger.Credit,
		initial, mustTestMoney(t, "0.00", money.BRL), initial, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wallets().Create(ctx, entity); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, opening); err != nil {
			return err
		}
		return repos.Ledger().Append(ctx, entry, 1)
	}); err != nil {
		t.Fatalf("persist wallet opening and ledger atomically: %v", err)
	}
}

func TestPostgresIdempotencyInboxAndDurableOutbox(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	entity, err := wallet.Open(testWalletID, testPlayerID, mustTestMoney(t, "0.00", money.BRL), now)
	if err != nil {
		t.Fatal(err)
	}
	transaction := newTestWager(t, testTransactionID, "external-1", "same-key", now)

	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wallets().Create(ctx, entity); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, transaction); err != nil {
			return err
		}
		receipt, err := repos.Inbox().Receive(ctx, "wager-consumer", "message-1", hashA, now)
		if err != nil {
			return err
		}
		if !receipt.Inserted || receipt.State != InboxReceived {
			return fmt.Errorf("new inbox receipt = %+v", receipt)
		}
		if err := repos.Inbox().Complete(ctx, "wager-consumer", "message-1", hashA, now.Add(time.Second)); err != nil {
			return err
		}
		return repos.Outbox().Append(ctx, OutboxEvent{
			EventID: testEventID, AggregateID: testWalletID, EventType: "TestEvent",
			SchemaVersion: 1, CorrelationID: testEventID,
			Payload:    json.RawMessage(`{"amount":"1.00","currency":"BRL"}`),
			OccurredAt: now, CreatedAt: now,
		})
	}); err != nil {
		t.Fatalf("persist idempotency/inbox/outbox: %v", err)
	}

	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		receipt, err := repos.Inbox().Receive(ctx, "wager-consumer", "message-1", hashA, now.Add(time.Second))
		if err != nil {
			return err
		}
		if receipt.Inserted || receipt.State != InboxCompleted {
			return fmt.Errorf("duplicate inbox receipt = %+v", receipt)
		}
		_, err = repos.Inbox().Receive(ctx, "wager-consumer", "message-1", hashB, now.Add(time.Second))
		if !errors.Is(err, ErrInboxHashMismatch) {
			return fmt.Errorf("changed inbox payload error = %v, want ErrInboxHashMismatch", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("check durable inbox replay: %v", err)
	}

	var record IdempotencyRecord
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		var err error
		record, err = repos.Idempotency().Find(ctx, "provider-a", "same-key")
		return err
	}); err != nil {
		t.Fatalf("find idempotency record: %v", err)
	}
	if record.TransactionID != testTransactionID || record.PayloadHash != hashA {
		t.Fatalf("idempotency record = %+v", record)
	}

	claim := func(owner string, at time.Time) ([]OutboxEvent, error) {
		var events []OutboxEvent
		err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
			var err error
			events, err = repos.Outbox().ClaimBatch(ctx, owner, at, time.Minute, 10)
			return err
		})
		return events, err
	}
	claimed, err := claim("worker-a", now.Add(2*time.Second))
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("first outbox claim = %+v, %v", claimed, err)
	}
	firstLeaseOwner := claimed[0].LockOwner
	claimed, err = claim("worker-b", now.Add(3*time.Second))
	if err != nil || len(claimed) != 0 {
		t.Fatalf("outbox claimed during active lease = %+v, %v", claimed, err)
	}
	claimed, err = claim("worker-b", now.Add(2*time.Minute))
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("outbox was not reclaimed after lease expiry = %+v, %v", claimed, err)
	}
	reclaimedLeaseOwner := claimed[0].LockOwner
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Outbox().MarkPublished(ctx, testEventID, firstLeaseOwner, now.Add(2*time.Minute))
	}); !errors.Is(err, ErrOutboxLeaseLost) {
		t.Fatalf("expired outbox owner mark error = %v, want ErrOutboxLeaseLost", err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Outbox().MarkPublished(ctx, testEventID, reclaimedLeaseOwner, now.Add(2*time.Minute))
	}); err != nil {
		t.Fatalf("mark outbox event published: %v", err)
	}
	claimed, err = claim("worker-c", now.Add(3*time.Minute))
	if err != nil || len(claimed) != 0 {
		t.Fatalf("published outbox event was claimed again = %+v, %v", claimed, err)
	}

	duplicateKey := newTestWager(t, testTransactionID2, "external-2", "same-key", now.Add(time.Second))
	err = uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Wagers().Create(ctx, duplicateKey)
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate idempotency key error = %v, want ErrConflict", err)
	}
	duplicateExternalID := newTestWager(t, testThirdTransactionID, "external-1", "different-key", now.Add(2*time.Second))
	err = uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Wagers().Create(ctx, duplicateExternalID)
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate external transaction id error = %v, want ErrConflict", err)
	}
}

func TestPostgresPendingReferenceIsDurableAndReversible(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	walletEntity, opening, openingEntry, bet, entry := financialFixture(t)
	refund, err := wager.NewExternal(wager.ExternalParams{
		ID: testTransactionID2, ProviderID: "provider-a", ExternalTransactionID: "external-refund",
		IdempotencyKey: "key-refund", PayloadHash: hashB, WalletID: testWalletID,
		PlayerID: testPlayerID, RoundID: "round-1", GameID: "game-1", Kind: wager.Refund,
		Amount:                         mustTestMoney(t, "10.00", money.BRL),
		ReferenceExternalTransactionID: "external-fixture", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := refund.WaitForReference(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wallets().Create(ctx, walletEntity); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, opening); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, openingEntry, 1); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, bet); err != nil {
			return err
		}
		if err := repos.Wallets().SaveBalance(ctx, walletEntity.ID(), walletEntity.Version(), entry.BalanceAfter(), 2, entry.CreatedAt()); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, entry, 2); err != nil {
			return err
		}
		return repos.Wagers().Create(ctx, refund)
	}); err != nil {
		t.Fatalf("persist reference-pending transaction: %v", err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		pending, err := repos.Wagers().FindPendingReferences(ctx, 10)
		if err != nil {
			return err
		}
		if len(pending) != 1 || pending[0].ID() != testTransactionID2 ||
			pending[0].Status() != wager.PendingReference {
			return fmt.Errorf("durable pending-reference query = %#v", pending)
		}
		return nil
	}); err != nil {
		t.Fatalf("query durable reference pending: %v", err)
	}

	processedRefund, err := wager.NewExternal(wager.ExternalParams{
		ID: testTransactionID2, ProviderID: "provider-a", ExternalTransactionID: "external-refund",
		IdempotencyKey: "key-refund", PayloadHash: hashB, WalletID: testWalletID,
		PlayerID: testPlayerID, RoundID: "round-1", GameID: "game-1", Kind: wager.Refund,
		Amount:                         mustTestMoney(t, "10.00", money.BRL),
		ReferenceExternalTransactionID: "external-fixture", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := processedRefund.Process(
		mustTestMoney(t, "90.00", money.BRL),
		mustTestMoney(t, "100.00", money.BRL),
		bet,
		now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wagers().UpdateState(ctx, processedRefund); err != nil {
			return err
		}
		return repos.Reversals().RecordProcessed(ctx, bet.ID(), processedRefund.ID(), now.Add(2*time.Second))
	}); err != nil {
		t.Fatalf("process and reserve reversal: %v", err)
	}

	secondReversal, err := wager.NewExternal(wager.ExternalParams{
		ID: testThirdTransactionID, ProviderID: "provider-a", ExternalTransactionID: "external-rollback",
		IdempotencyKey: "key-rollback", PayloadHash: hashA, WalletID: testWalletID,
		PlayerID: testPlayerID, RoundID: "round-1", GameID: "game-1", Kind: wager.Rollback,
		Amount:                         mustTestMoney(t, "10.00", money.BRL),
		ReferenceExternalTransactionID: "external-fixture", CreatedAt: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondReversal.Process(
		mustTestMoney(t, "100.00", money.BRL),
		mustTestMoney(t, "110.00", money.BRL),
		bet,
		now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wagers().Create(ctx, secondReversal); err != nil {
			return err
		}
		return repos.Reversals().RecordProcessed(ctx, bet.ID(), secondReversal.ID(), now.Add(4*time.Second))
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second reversal error = %v, want ErrConflict", err)
	}
	var reversalID string
	if err := pool.QueryRow(ctx, `SELECT reversal_transaction_id::text FROM wager_reversals WHERE reference_transaction_id = $1`, bet.ID()).Scan(&reversalID); err != nil {
		t.Fatal(err)
	}
	if reversalID != testTransactionID2 {
		t.Fatalf("persisted reversal id = %s, want %s", reversalID, testTransactionID2)
	}
}

func TestPostgresUnitOfWorkRollsBackAllFinancialWrites(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx := context.Background()
	walletEntity, opening, openingEntry, transaction, entry := financialFixture(t)
	injected := errors.New("abort financial operation")
	err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wallets().Create(ctx, walletEntity); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, opening); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, openingEntry, 1); err != nil {
			return err
		}
		if err := repos.Wagers().Create(ctx, transaction); err != nil {
			return err
		}
		if err := repos.Wallets().SaveBalance(ctx, walletEntity.ID(), walletEntity.Version(), entry.BalanceAfter(), 2, entry.CreatedAt()); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, entry, 2); err != nil {
			return err
		}
		if _, err := repos.Inbox().Receive(ctx, "rollback-consumer", "message-rollback", hashA, entry.CreatedAt()); err != nil {
			return err
		}
		if err := repos.Outbox().Append(ctx, OutboxEvent{
			EventID: testEventID, AggregateID: testWalletID, EventType: "TestEvent",
			SchemaVersion: 1, CorrelationID: testEventID,
			Payload: json.RawMessage(`{"ok":true}`), OccurredAt: entry.CreatedAt(),
			CreatedAt: entry.CreatedAt(),
		}); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("Within() error = %v, want injected failure", err)
	}
	for _, table := range []string{
		"wallets", "wager_transactions", "idempotency_records",
		"wallet_ledger_entries", "inbox_messages", "outbox_events",
	} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s rows after rollback = %d, want 0", table, count)
		}
	}
}

func TestPostgresWalletRowLockSerializesIndependentTransactions(t *testing.T) {
	pool := integrationPool(t)
	uow := NewUnitOfWork(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entity, err := wallet.Open(testWalletID, testPlayerID, mustTestMoney(t, "0.00", money.BRL), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		return repos.Wallets().Create(ctx, entity)
	}); err != nil {
		t.Fatal(err)
	}

	firstHasLock := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
			if _, err := repos.Wallets().LockByID(ctx, testWalletID); err != nil {
				return err
			}
			close(firstHasLock)
			<-releaseFirst
			return nil
		})
	}()
	<-firstHasLock

	secondStarted := make(chan struct{})
	secondAcquired := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- uow.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
			close(secondStarted)
			if _, err := repos.Wallets().LockByID(ctx, testWalletID); err != nil {
				return err
			}
			close(secondAcquired)
			return nil
		})
	}()
	<-secondStarted
	select {
	case <-secondAcquired:
		t.Fatal("second transaction acquired the wallet row lock before the first committed")
	case <-time.After(150 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseFirst) })
	if err := <-firstDone; err != nil {
		t.Fatalf("first transaction: %v", err)
	}
	select {
	case <-secondAcquired:
	case <-ctx.Done():
		t.Fatal("second transaction did not acquire released wallet lock")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second transaction: %v", err)
	}
}

const (
	testWalletID             = "0192f299-27dd-7d3f-8071-5f8685deef35"
	testWalletID2            = "0192f299-27dd-7d3f-8071-5f8685deef36"
	testPlayerID             = "0192f299-27dd-7d3f-8071-5f8685deef37"
	testTransactionID        = "0192f299-27dd-7d3f-8071-5f8685deef38"
	testTransactionID2       = "0192f299-27dd-7d3f-8071-5f8685deef39"
	testThirdTransactionID   = "0192f299-27dd-7d3f-8071-5f8685deef43"
	testOpeningTransactionID = "0192f299-27dd-7d3f-8071-5f8685deef44"
	testLedgerID             = "0192f299-27dd-7d3f-8071-5f8685deef40"
	testLedgerID2            = "0192f299-27dd-7d3f-8071-5f8685deef41"
	testOpeningLedgerID      = "0192f299-27dd-7d3f-8071-5f8685deef45"
	testEventID              = "0192f299-27dd-7d3f-8071-5f8685deef42"
	hashA                    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB                    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL integration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping PostgreSQL integration database: %v", err)
	}
	schemaBytes := make([]byte, 12)
	if _, err := rand.Read(schemaBytes); err != nil {
		admin.Close()
		t.Fatalf("generate temporary schema name: %v", err)
	}
	schema := "jungle_it_" + hex.EncodeToString(schemaBytes)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatalf("create isolated integration schema: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatalf("parse PostgreSQL integration URL: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatalf("open isolated PostgreSQL pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop isolated integration schema: %v", err)
		}
		admin.Close()
	})
	if err := NewMigrationRunner(pool, migrationfiles.Files).Up(ctx); err != nil {
		t.Fatalf("apply migrations to isolated integration schema: %v", err)
	}
	return pool
}

func financialFixture(t *testing.T) (*wallet.Wallet, *wager.WagerTransaction, ledger.WalletLedgerEntry, *wager.WagerTransaction, ledger.WalletLedgerEntry) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entity, err := wallet.Open(testWalletID, testPlayerID, mustTestMoney(t, "100.00", money.BRL), now)
	if err != nil {
		t.Fatal(err)
	}
	opening, err := wager.NewOpening(wager.OpeningParams{
		ID: testOpeningTransactionID, WalletID: testWalletID, PlayerID: testPlayerID,
		Amount: mustTestMoney(t, "100.00", money.BRL), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	openingEntry, err := ledger.NewEntry(
		testOpeningLedgerID, testWalletID, testOpeningTransactionID, ledger.Credit,
		mustTestMoney(t, "100.00", money.BRL), mustTestMoney(t, "0.00", money.BRL),
		mustTestMoney(t, "100.00", money.BRL), now)
	if err != nil {
		t.Fatal(err)
	}
	transaction := newTestWager(t, testTransactionID, "external-fixture", "key-fixture", now)
	if err := transaction.Process(
		mustTestMoney(t, "100.00", money.BRL),
		mustTestMoney(t, "90.00", money.BRL),
		nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	entry, err := ledger.NewEntry(testLedgerID, testWalletID, testTransactionID, ledger.Debit,
		mustTestMoney(t, "10.00", money.BRL), mustTestMoney(t, "100.00", money.BRL),
		mustTestMoney(t, "90.00", money.BRL), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return entity, opening, openingEntry, transaction, entry
}

func newTestWager(t *testing.T, id, externalID, idempotencyKey string, createdAt time.Time) *wager.WagerTransaction {
	t.Helper()
	transaction, err := wager.NewExternal(wager.ExternalParams{
		ID: id, ProviderID: "provider-a", ExternalTransactionID: externalID,
		IdempotencyKey: idempotencyKey, PayloadHash: hashA,
		WalletID: testWalletID, PlayerID: testPlayerID, RoundID: "round-1", GameID: "game-1",
		Kind: wager.Bet, Amount: mustTestMoney(t, "10.00", money.BRL), CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create test wager: %v", err)
	}
	return transaction
}

func mustTestMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	value, err := money.ParseAmount(amount, currency)
	if err != nil {
		t.Fatalf("parse test money %s: %v", amount, err)
	}
	return value
}

func isConstraintError(err error) bool {
	if errors.Is(err, ErrConstraintViolation) || errors.Is(err, ErrConflict) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code[:2] == "23" || pgErr.Code == "22003")
}
