package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
)

type resolverUnitOfWork struct {
	repositories port.Repositories
}

func (u resolverUnitOfWork) Within(ctx context.Context, fn func(context.Context, port.Repositories) error) error {
	return fn(ctx, u.repositories)
}

type resolverRepositories struct {
	wagers port.WagerRepository
}

func (r resolverRepositories) Wallets() port.WalletRepository          { return nil }
func (r resolverRepositories) Wagers() port.WagerRepository            { return r.wagers }
func (r resolverRepositories) Ledger() port.LedgerRepository           { return nil }
func (r resolverRepositories) Idempotency() port.IdempotencyRepository { return nil }
func (r resolverRepositories) Inbox() port.InboxRepository             { return nil }
func (r resolverRepositories) Outbox() port.OutboxRepository           { return nil }
func (r resolverRepositories) Reversals() port.ReversalRepository      { return nil }

type resolverWagers struct {
	called chan struct{}
	once   sync.Once
}

func (r *resolverWagers) Create(context.Context, *wager.WagerTransaction) error { return nil }
func (r *resolverWagers) FindByID(context.Context, string) (*wager.WagerTransaction, error) {
	return nil, port.ErrNotFound
}
func (r *resolverWagers) LockByID(context.Context, string) (*wager.WagerTransaction, error) {
	return nil, port.ErrNotFound
}
func (r *resolverWagers) FindByExternalID(context.Context, string, string) (*wager.WagerTransaction, error) {
	return nil, port.ErrNotFound
}
func (r *resolverWagers) FindByIdempotencyKey(context.Context, string, string) (*wager.WagerTransaction, error) {
	return nil, port.ErrNotFound
}
func (r *resolverWagers) UpdateState(context.Context, *wager.WagerTransaction) error { return nil }
func (r *resolverWagers) FindPendingReferences(context.Context, int) ([]*wager.WagerTransaction, error) {
	r.once.Do(func() { close(r.called) })
	return nil, nil
}
func (r *resolverWagers) SchedulePendingReferenceCheck(context.Context, string, time.Time) error {
	return nil
}

func TestReferenceResolverFxLifecycleCancelsAndWaits(t *testing.T) {
	wagers := &resolverWagers{called: make(chan struct{})}
	service := application.NewFinancialService(resolverUnitOfWork{
		repositories: resolverRepositories{wagers: wagers},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{ShutdownTimeout: time.Second}
	resolver := NewReferenceResolver(service, logger)
	app := fx.New(
		fx.Provide(func() *ReferenceResolver { return resolver }),
		fx.Provide(func() config.Config { return cfg }),
		fx.Provide(func() *slog.Logger { return logger }),
		fx.Invoke(RegisterReferenceResolver),
		fx.NopLogger,
	)
	startCtx, cancelStart := context.WithTimeout(context.Background(), time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start Fx reference resolver: %v", err)
	}
	select {
	case <-wagers.called:
	case <-startCtx.Done():
		_ = app.Stop(context.Background())
		t.Fatal("reference resolver did not query pending records after Fx startup")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("stop Fx reference resolver: %v", err)
	}
}

var _ port.UnitOfWork = resolverUnitOfWork{}
var _ port.Repositories = resolverRepositories{}
var _ port.WagerRepository = (*resolverWagers)(nil)
