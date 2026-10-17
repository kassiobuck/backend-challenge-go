package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
	"github.com/kassiobuck/backend-challenge-go/internal/health"
)

func NewPool(cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	return pool, nil
}

type PoolLifecycle interface {
	Ping(context.Context) error
	Close()
}

func RegisterPoolLifecycle(
	lifecycle fx.Lifecycle,
	pool PoolLifecycle,
	logger *slog.Logger,
	cfg config.Config,
) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
			defer cancel()
			if err := pool.Ping(startupCtx); err != nil {
				pool.Close()
				return fmt.Errorf("check PostgreSQL during startup: %w", err)
			}
			logger.Info("PostgreSQL connection ready")
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			logger.Info("PostgreSQL pool closed")
			return nil
		},
	})
}

type Probe struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

func NewProbe(pool *pgxpool.Pool) *Probe {
	return &Probe{pool: pool, timeout: 2 * time.Second}
}

func (p *Probe) Name() string {
	return "postgresql"
}

func (p *Probe) Check(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	return p.pool.Ping(checkCtx)
}

var _ health.Probe = (*Probe)(nil)
