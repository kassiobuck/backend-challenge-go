package sqsout

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

type lifecycleUnitOfWork struct {
	repositories port.Repositories
}

func (u lifecycleUnitOfWork) Within(ctx context.Context, fn func(context.Context, port.Repositories) error) error {
	return fn(ctx, u.repositories)
}

type lifecycleRepositories struct {
	outbox port.OutboxRepository
}

func (r lifecycleRepositories) Wallets() port.WalletRepository          { return nil }
func (r lifecycleRepositories) Wagers() port.WagerRepository            { return nil }
func (r lifecycleRepositories) Ledger() port.LedgerRepository           { return nil }
func (r lifecycleRepositories) Idempotency() port.IdempotencyRepository { return nil }
func (r lifecycleRepositories) Inbox() port.InboxRepository             { return nil }
func (r lifecycleRepositories) Outbox() port.OutboxRepository           { return r.outbox }
func (r lifecycleRepositories) Reversals() port.ReversalRepository      { return nil }

type lifecycleOutbox struct {
	called chan struct{}
	once   sync.Once
}

func (o *lifecycleOutbox) Append(context.Context, port.OutboxEvent) error { return nil }
func (o *lifecycleOutbox) ClaimBatch(context.Context, string, time.Time, time.Duration, int) ([]port.OutboxEvent, error) {
	o.once.Do(func() { close(o.called) })
	return nil, nil
}
func (o *lifecycleOutbox) MarkPublished(context.Context, string, string, time.Time) error {
	return nil
}
func (o *lifecycleOutbox) Reschedule(context.Context, string, string, string, time.Time) error {
	return nil
}

func TestPublisherFxLifecycleCancelsAndWaitsForPolling(t *testing.T) {
	outbox := &lifecycleOutbox{called: make(chan struct{})}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{ShutdownTimeout: time.Second}
	publisher, err := NewPublisher(
		lifecycleUnitOfWork{repositories: lifecycleRepositories{outbox: outbox}},
		nil, config.Config{SQSOutputQueueURL: "http://localhost/events.fifo"}, logger)
	if err != nil {
		t.Fatalf("NewPublisher(): %v", err)
	}
	app := fx.New(
		fx.Provide(func() *Publisher { return publisher }),
		fx.Provide(func() config.Config { return cfg }),
		fx.Provide(func() *slog.Logger { return logger }),
		fx.Invoke(RegisterPublisher),
		fx.NopLogger,
	)
	startCtx, cancelStart := context.WithTimeout(context.Background(), time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start Fx publisher: %v", err)
	}
	select {
	case <-outbox.called:
	case <-startCtx.Done():
		_ = app.Stop(context.Background())
		t.Fatal("outbox publisher did not claim work after Fx startup")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("stop Fx publisher: %v", err)
	}
}

var _ port.UnitOfWork = lifecycleUnitOfWork{}
var _ port.Repositories = lifecycleRepositories{}
var _ port.OutboxRepository = (*lifecycleOutbox)(nil)
