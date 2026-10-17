package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/application"
	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type ReadinessChecker interface {
	Check(context.Context) map[string]string
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewHandler(
	checker ReadinessChecker,
	logger *slog.Logger,
	financial *application.FinancialService,
	verifier port.TokenVerifier,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLiveness)
	mux.HandleFunc("GET /health/ready", newReadinessHandler(checker))
	registerBusinessRoutes(mux, financial, verifier, logger)
	return requestLogger(logger, mux)
}

func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func newReadinessHandler(checker ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		dependencies := checker.Check(ctx)
		ready := true
		for _, status := range dependencies {
			if status != "ok" {
				ready = false
				break
			}
		}

		status := http.StatusOK
		responseStatus := "ready"
		if !ready {
			status = http.StatusServiceUnavailable
			responseStatus = "not_ready"
		}

		writeJSON(w, status, struct {
			Status       string            `json:"status"`
			Dependencies map[string]string `json:"dependencies"`
		}{
			Status:       responseStatus,
			Dependencies: dependencies,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("write HTTP response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &statusWriter{ResponseWriter: w}
		startedAt := time.Now()
		next.ServeHTTP(writer, r)
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}

		logger.Info("HTTP request",
			"method", r.Method,
			"status", status,
			"duration", time.Since(startedAt),
		)
	})
}
