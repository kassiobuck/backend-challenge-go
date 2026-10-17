package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

func TestHTTPServerShutdownReleasesListener(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener address: %v", err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &http.Server{
		Addr:              address,
		Handler:           NewHandler(readinessStub{"postgresql": "ok", "sqs": "ok"}, logger, nil, nil),
		ReadHeaderTimeout: time.Second,
	}
	app := fx.New(
		fx.Supply(server, logger, config.Config{ShutdownTimeout: time.Second}),
		fx.Invoke(RegisterHTTPServer),
		fx.NopLogger,
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("app.Start() error = %v", err)
	}

	response, err := http.Get("http://" + address + "/health/live")
	if err != nil {
		_ = app.Stop(ctx)
		t.Fatalf("GET /health/live: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = app.Stop(ctx)
		t.Fatalf("health status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	if err := app.Stop(ctx); err != nil {
		t.Fatalf("app.Stop() error = %v", err)
	}

	released, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listener address was not released: %v", err)
	}
	_ = released.Close()
}
