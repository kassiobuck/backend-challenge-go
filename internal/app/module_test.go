package app

import (
	"context"
	"testing"

	"go.uber.org/fx"
)

func TestModuleDependencyGraphIsValid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://jungle:local-only@localhost:5432/jungle?sslmode=disable")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:4566")
	t.Setenv("SQS_QUEUE_URL", "http://localhost:4566/000000000000/wager-transactions.fifo")

	if err := fx.ValidateApp(Module, fx.NopLogger); err != nil {
		t.Fatalf("Fx module graph is invalid: %v", err)
	}
}

func TestInvalidConfigurationPreventsApplicationStartup(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("SQS_QUEUE_URL", "")

	app := fx.New(Module, fx.NopLogger)

	if err := app.Start(context.Background()); err == nil {
		_ = app.Stop(context.Background())
		t.Fatal("app.Start() succeeded with invalid configuration")
	}
}
