package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/messaging"
)

type ReferenceResolver struct {
	service  *application.FinancialService
	logger   *slog.Logger
	interval time.Duration
	batch    int
}

func NewReferenceResolver(service *application.FinancialService, logger *slog.Logger) *ReferenceResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReferenceResolver{service: service, logger: logger, interval: time.Second, batch: 100}
}

func (w *ReferenceResolver) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		count, err := w.service.ResolvePendingReferences(ctx, w.batch)
		delay := w.interval
		if err != nil && ctx.Err() == nil {
			failures++
			w.logger.Error("resolve pending wager references failed", "error", err)
		} else if count > 0 {
			failures = 0
			w.logger.Info("pending wager references resolved", "count", count)
		} else {
			failures = 0
		}
		if err != nil {
			delay = messaging.Backoff(failures, time.Second, time.Minute)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func RegisterReferenceResolver(
	lifecycle fx.Lifecycle,
	resolver *ReferenceResolver,
	cfg config.Config,
	logger *slog.Logger,
) {
	var cancel context.CancelFunc
	var done chan struct{}
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			workerCtx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			done = make(chan struct{})
			go func() {
				defer close(done)
				resolver.Run(workerCtx)
			}()
			logger.Info("pending reference resolver started")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if cancel == nil {
				return nil
			}
			cancel()
			stopCtx, stopCancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
			defer stopCancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return fmt.Errorf("stop reference resolver within %s: %w", cfg.ShutdownTimeout, stopCtx.Err())
			}
		},
	})
}
