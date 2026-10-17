package postgres

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

type poolStub struct {
	pingCalls  int
	closeCalls int
	pingErr    error
}

func (p *poolStub) Ping(context.Context) error {
	p.pingCalls++
	return p.pingErr
}

func TestPoolLifecycleClosesResourceWhenStartupCheckFails(t *testing.T) {
	pool := &poolStub{pingErr: errors.New("database unavailable")}
	cfg := config.Config{StartupTimeout: time.Second}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := fx.New(
		fx.Provide(func() PoolLifecycle { return pool }),
		fx.Supply(cfg, logger),
		fx.Invoke(RegisterPoolLifecycle),
		fx.NopLogger,
	)

	if err := app.Start(context.Background()); err == nil {
		_ = app.Stop(context.Background())
		t.Fatal("app.Start() succeeded despite failed PostgreSQL check")
	}
	if pool.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1 after startup failure", pool.closeCalls)
	}
}

func (p *poolStub) Close() {
	p.closeCalls++
}

func TestPoolLifecyclePingsAndClosesResource(t *testing.T) {
	pool := &poolStub{}
	cfg := config.Config{StartupTimeout: time.Second}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := fx.New(
		fx.Provide(func() PoolLifecycle { return pool }),
		fx.Supply(cfg, logger),
		fx.Invoke(RegisterPoolLifecycle),
		fx.NopLogger,
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("app.Start() error = %v", err)
	}
	if pool.pingCalls != 1 {
		t.Fatalf("Ping calls = %d, want 1", pool.pingCalls)
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("app.Stop() error = %v", err)
	}
	if pool.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", pool.closeCalls)
	}
}
