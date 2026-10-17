package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/config"
)

func NewHTTPServer(
	cfg config.Config,
	checker ReadinessChecker,
	logger *slog.Logger,
	financial *application.FinancialService,
	verifier port.TokenVerifier,
) *http.Server {
	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           NewHandler(checker, logger, financial, verifier),
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func RegisterHTTPServer(
	lifecycle fx.Lifecycle,
	server *http.Server,
	logger *slog.Logger,
	shutdowner fx.Shutdowner,
	cfg config.Config,
) {
	serveResult := make(chan error, 1)
	var listener net.Listener

	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			openedListener, err := net.Listen("tcp", server.Addr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", server.Addr, err)
			}

			listener = openedListener
			logger.Info("HTTP server started", "address", listener.Addr().String())
			go func() {
				err := server.Serve(listener)
				serveResult <- err
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("HTTP server stopped unexpectedly", "error", err)
					if shutdownErr := shutdowner.Shutdown(fx.ExitCode(1)); shutdownErr != nil {
						logger.Error("request application shutdown", "error", shutdownErr)
					}
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutdownCtx, cancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
			defer cancel()

			shutdownErr := server.Shutdown(shutdownCtx)
			if shutdownErr != nil {
				shutdownErr = errors.Join(shutdownErr, server.Close())
			}

			serveErr := <-serveResult
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				serveErr = fmt.Errorf("serve HTTP: %w", serveErr)
			} else {
				serveErr = nil
			}

			if shutdownErr == nil && serveErr == nil {
				logger.Info("HTTP server stopped")
			}
			return errors.Join(shutdownErr, serveErr)
		},
	})
}
