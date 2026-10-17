package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

type LedgerRepository struct {
	tx pgx.Tx
}

func (r *LedgerRepository) Append(ctx context.Context, entry ledger.WalletLedgerEntry, walletVersion uint64) error {
	amount, err := entry.Amount().MinorUnits()
	if err != nil {
		return fmt.Errorf("read ledger amount: %w", err)
	}
	before, err := entry.BalanceBefore().MinorUnits()
	if err != nil {
		return fmt.Errorf("read ledger prior balance: %w", err)
	}
	after, err := entry.BalanceAfter().MinorUnits()
	if err != nil {
		return fmt.Errorf("read ledger resulting balance: %w", err)
	}
	currency, err := currencyCode(entry.Amount())
	if err != nil {
		return fmt.Errorf("read ledger currency: %w", err)
	}
	version, err := postgresVersion(walletVersion)
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(ctx, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, wallet_version, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		entry.ID(), entry.WalletID(), entry.TransactionID(), entry.Direction(),
		amount, currency, before, after, version, entry.CreatedAt())
	return mapConstraintError(err)
}

func (r *LedgerRepository) FindByTransactionID(ctx context.Context, walletID, transactionID string) (ledger.WalletLedgerEntry, error) {
	return r.find(ctx, `WHERE wallet_id = $1 AND transaction_id = $2`, walletID, transactionID)
}

func (r *LedgerRepository) List(ctx context.Context, walletID string, before *LedgerCursor, limit int) ([]ledger.WalletLedgerEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("ledger limit must be positive")
	}
	var (
		rows pgx.Rows
		err  error
	)
	if before == nil {
		rows, err = r.tx.Query(ctx, `
			SELECT id::text, wallet_id::text, transaction_id::text, direction,
				amount_minor, currency, balance_before_minor, balance_after_minor, created_at
			FROM wallet_ledger_entries WHERE wallet_id = $1
			ORDER BY created_at DESC, id DESC LIMIT $2`, walletID, limit)
	} else {
		rows, err = r.tx.Query(ctx, `
			SELECT id::text, wallet_id::text, transaction_id::text, direction,
				amount_minor, currency, balance_before_minor, balance_after_minor, created_at
			FROM wallet_ledger_entries
			WHERE wallet_id = $1 AND (created_at, id) < ($2, $3::uuid)
			ORDER BY created_at DESC, id DESC LIMIT $4`,
			walletID, before.CreatedAt, before.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query wallet ledger: %w", err)
	}
	defer rows.Close()
	entries := make([]ledger.WalletLedgerEntry, 0)
	for rows.Next() {
		entry, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate wallet ledger: %w", err)
	}
	return entries, nil
}

func (r *LedgerRepository) Reconcile(ctx context.Context, walletID string) (port.LedgerTotals, error) {
	var (
		creditsText string
		debitsText  string
		totals      port.LedgerTotals
	)
	err := r.tx.QueryRow(ctx, `
		SELECT
			COALESCE(sum(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0)::text,
			COALESCE(sum(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0)::text,
			count(*)
		FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).
		Scan(&creditsText, &debitsText, &totals.EntryCount)
	if err != nil {
		return port.LedgerTotals{}, fmt.Errorf("aggregate wallet ledger: %w", err)
	}
	if _, err := fmt.Sscan(creditsText, &totals.CreditsMinor); err != nil {
		return port.LedgerTotals{}, fmt.Errorf("decode ledger credit total: %w", err)
	}
	if _, err := fmt.Sscan(debitsText, &totals.DebitsMinor); err != nil {
		return port.LedgerTotals{}, fmt.Errorf("decode ledger debit total: %w", err)
	}
	return totals, nil
}

func (r *LedgerRepository) find(ctx context.Context, predicate string, args ...any) (ledger.WalletLedgerEntry, error) {
	row := r.tx.QueryRow(ctx, `
		SELECT id::text, wallet_id::text, transaction_id::text, direction,
			amount_minor, currency, balance_before_minor, balance_after_minor, created_at
		FROM wallet_ledger_entries `+predicate, args...)
	entry, err := scanLedgerRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ledger.WalletLedgerEntry{}, ErrNotFound
		}
		return ledger.WalletLedgerEntry{}, err
	}
	return entry, nil
}

type LedgerCursor = port.LedgerCursor

type ledgerScanner interface {
	Scan(dest ...any) error
}

func scanLedgerEntry(rows pgx.Rows) (ledger.WalletLedgerEntry, error) {
	return scanLedgerRow(rows)
}

func scanLedgerRow(row ledgerScanner) (ledger.WalletLedgerEntry, error) {
	var (
		id, walletID, transactionID, direction, currencyCode string
		amount, before, after                                int64
		createdAt                                            time.Time
	)
	if err := row.Scan(&id, &walletID, &transactionID, &direction,
		&amount, &currencyCode, &before, &after, &createdAt); err != nil {
		return ledger.WalletLedgerEntry{}, fmt.Errorf("scan ledger entry: %w", err)
	}
	currency, err := money.NewCurrency(currencyCode)
	if err != nil {
		return ledger.WalletLedgerEntry{}, fmt.Errorf("%w: ledger currency", ErrCorruptRow)
	}
	amountValue, err := money.NewMinor(amount, currency)
	if err != nil {
		return ledger.WalletLedgerEntry{}, fmt.Errorf("%w: ledger amount", ErrCorruptRow)
	}
	beforeValue, err := money.NewMinor(before, currency)
	if err != nil {
		return ledger.WalletLedgerEntry{}, fmt.Errorf("%w: ledger prior balance", ErrCorruptRow)
	}
	afterValue, err := money.NewMinor(after, currency)
	if err != nil {
		return ledger.WalletLedgerEntry{}, fmt.Errorf("%w: ledger resulting balance", ErrCorruptRow)
	}
	entry, err := ledger.NewEntry(id, walletID, transactionID, ledger.Direction(direction),
		amountValue, beforeValue, afterValue, createdAt)
	if err != nil {
		return ledger.WalletLedgerEntry{}, fmt.Errorf("rehydrate ledger entry: %w: %v", ErrCorruptRow, err)
	}
	return entry, nil
}
