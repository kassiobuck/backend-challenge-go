package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wallet"
)

type WalletRepository struct {
	tx pgx.Tx
}

func (r *WalletRepository) Create(ctx context.Context, entity *wallet.Wallet) error {
	if entity == nil {
		return errors.New("cannot persist nil wallet")
	}
	minor, err := entity.Balance().MinorUnits()
	if err != nil {
		return fmt.Errorf("read wallet balance: %w", err)
	}
	version, err := postgresVersion(entity.Version())
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		entity.ID(), entity.PlayerID(), entity.Currency(), minor, version,
		entity.CreatedAt(), entity.UpdatedAt())
	return mapConstraintError(err)
}

func (r *WalletRepository) FindByID(ctx context.Context, id string) (*wallet.Wallet, error) {
	return r.find(ctx, `WHERE id = $1`, id)
}

func (r *WalletRepository) FindByPlayerCurrency(ctx context.Context, playerID string, currency money.Currency) (*wallet.Wallet, error) {
	return r.find(ctx, `WHERE player_id = $1 AND currency = $2`, playerID, currency)
}

func (r *WalletRepository) LockByID(ctx context.Context, id string) (*wallet.Wallet, error) {
	return r.find(ctx, `WHERE id = $1 FOR UPDATE`, id)
}

func (r *WalletRepository) SaveBalance(
	ctx context.Context,
	id string,
	expectedVersion uint64,
	balance money.Money,
	newVersion uint64,
	updatedAt time.Time,
) error {
	minor, err := balance.MinorUnits()
	if err != nil {
		return fmt.Errorf("read next wallet balance: %w", err)
	}
	expected, err := postgresVersion(expectedVersion)
	if err != nil {
		return err
	}
	next, err := postgresVersion(newVersion)
	if err != nil {
		return err
	}
	tag, err := r.tx.Exec(ctx, `
		UPDATE wallets
		SET balance_minor = $1, version = $2, updated_at = $3
		WHERE id = $4 AND version = $5`,
		minor, next, updatedAt.UTC(), id, expected)
	if err != nil {
		return mapConstraintError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOptimisticLock
	}
	return nil
}

func postgresVersion(version uint64) (int64, error) {
	if version > math.MaxInt64 {
		return 0, domain.ErrVersionOverflow
	}
	return int64(version), nil
}

func (r *WalletRepository) find(ctx context.Context, predicate string, args ...any) (*wallet.Wallet, error) {
	var (
		id, playerID, currencyCode string
		balance                    int64
		version                    int64
		createdAt, updatedAt       time.Time
	)
	err := r.tx.QueryRow(ctx, `
		SELECT id::text, player_id::text, currency, balance_minor, version, created_at, updated_at
		FROM wallets `+predicate, args...).Scan(
		&id, &playerID, &currencyCode, &balance, &version, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query wallet: %w", err)
	}
	currency, err := money.NewCurrency(currencyCode)
	if err != nil {
		return nil, fmt.Errorf("decode wallet currency: %w", err)
	}
	amount, err := money.NewMinor(balance, currency)
	if err != nil {
		return nil, fmt.Errorf("decode wallet balance: %w", err)
	}
	if version <= 0 {
		return nil, fmt.Errorf("decode wallet version %d: %w", version, ErrCorruptRow)
	}
	entity, err := wallet.Rehydrate(id, playerID, currency, amount, uint64(version), createdAt, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("rehydrate wallet: %w", err)
	}
	return entity, nil
}
