package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type UnitOfWork struct {
	pool *pgxpool.Pool
}

func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{pool: pool}
}

type repositories struct {
	wallets     *WalletRepository
	wagers      *WagerRepository
	ledger      *LedgerRepository
	idempotency *IdempotencyRepository
	inbox       *InboxRepository
	outbox      *OutboxRepository
	reversals   *ReversalRepository
}

func (u *UnitOfWork) Within(ctx context.Context, fn func(context.Context, port.Repositories) error) error {
	if fn == nil {
		return errors.New("unit of work callback is nil")
	}
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	repositories := newRepositories(tx)
	if err := fn(ctx, repositories); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled before PostgreSQL commit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL transaction: %w", mapConstraintError(err))
	}
	return nil
}

func newRepositories(tx pgx.Tx) *repositories {
	return &repositories{
		wallets:     &WalletRepository{tx: tx},
		wagers:      &WagerRepository{tx: tx},
		ledger:      &LedgerRepository{tx: tx},
		idempotency: &IdempotencyRepository{tx: tx},
		inbox:       &InboxRepository{tx: tx},
		outbox:      &OutboxRepository{tx: tx},
		reversals:   &ReversalRepository{tx: tx},
	}
}

func (r *repositories) Wallets() port.WalletRepository          { return r.wallets }
func (r *repositories) Wagers() port.WagerRepository            { return r.wagers }
func (r *repositories) Ledger() port.LedgerRepository           { return r.ledger }
func (r *repositories) Idempotency() port.IdempotencyRepository { return r.idempotency }
func (r *repositories) Inbox() port.InboxRepository             { return r.inbox }
func (r *repositories) Outbox() port.OutboxRepository           { return r.outbox }
func (r *repositories) Reversals() port.ReversalRepository      { return r.reversals }

var _ port.UnitOfWork = (*UnitOfWork)(nil)
var _ port.Repositories = (*repositories)(nil)
