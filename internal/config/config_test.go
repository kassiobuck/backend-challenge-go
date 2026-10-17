package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadFromValidEnvironment(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"DATABASE_URL":          "postgres://jungle:local-only@localhost:5432/jungle?sslmode=disable",
		"AWS_ACCESS_KEY_ID":     "test",
		"AWS_SECRET_ACCESS_KEY": "test",
		"AWS_ENDPOINT_URL":      "http://localhost:4566",
		"SQS_QUEUE_URL":         "http://localhost:4566/000000000000/wager-transactions.fifo",
	})

	cfg, err := LoadFrom(lookup)
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.SQSOutputQueueURL != "http://localhost:4566/000000000000/wager-events.fifo" ||
		cfg.SQSDLQURL != "http://localhost:4566/000000000000/wager-transactions-dlq.fifo" {
		t.Errorf("default output/DLQ URLs = %q/%q", cfg.SQSOutputQueueURL, cfg.SQSDLQURL)
	}
	if cfg.StartupTimeout != 10*time.Second || cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("default timeouts = %s/%s, want 10s/10s", cfg.StartupTimeout, cfg.ShutdownTimeout)
	}
}

func TestLoadFromRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "missing required setting",
			env:  map[string]string{},
			want: "DATABASE_URL is required",
		},
		{
			name: "invalid database URL",
			env: map[string]string{
				"DATABASE_URL":          "http://localhost/database",
				"AWS_ACCESS_KEY_ID":     "test",
				"AWS_SECRET_ACCESS_KEY": "test",
				"AWS_ENDPOINT_URL":      "http://localhost:4566",
				"SQS_QUEUE_URL":         "http://localhost:4566/queue",
			},
			want: "DATABASE_URL must be a PostgreSQL URL",
		},
		{
			name: "invalid startup timeout",
			env: map[string]string{
				"DATABASE_URL":          "postgres://localhost/jungle",
				"AWS_ACCESS_KEY_ID":     "test",
				"AWS_SECRET_ACCESS_KEY": "test",
				"AWS_ENDPOINT_URL":      "http://localhost:4566",
				"SQS_QUEUE_URL":         "http://localhost:4566/queue",
				"STARTUP_TIMEOUT":       "not-a-duration",
			},
			want: "STARTUP_TIMEOUT must be a valid duration",
		},
		{
			name: "invalid HTTP address",
			env: map[string]string{
				"DATABASE_URL":          "postgres://localhost/jungle",
				"AWS_ACCESS_KEY_ID":     "test",
				"AWS_SECRET_ACCESS_KEY": "test",
				"AWS_ENDPOINT_URL":      "http://localhost:4566",
				"SQS_QUEUE_URL":         "http://localhost:4566/queue",
				"HTTP_ADDR":             "not-an-address",
			},
			want: "HTTP_ADDR must be a host:port address",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFrom(testEnvironment(test.env))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadFrom() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadFromRejectsInvalidOIDCConfiguration(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":          "postgres://localhost/jungle",
		"AWS_ACCESS_KEY_ID":     "test",
		"AWS_SECRET_ACCESS_KEY": "test",
		"AWS_ENDPOINT_URL":      "http://localhost:4566",
		"SQS_QUEUE_URL":         "http://localhost:4566/queue",
	}
	tests := []struct {
		name string
		set  map[string]string
		want string
	}{
		{name: "invalid issuer", set: map[string]string{"OIDC_ISSUER_URL": "not-a-url"}, want: "OIDC_ISSUER_URL"},
		{name: "blank audience", set: map[string]string{"OIDC_AUDIENCE": " \t "}, want: "OIDC_AUDIENCE"},
		{name: "invalid internal URL", set: map[string]string{"OIDC_INTERNAL_URL": "ftp://keycloak"}, want: "OIDC_INTERNAL_URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := make(map[string]string, len(base)+len(test.set))
			for key, value := range base {
				values[key] = value
			}
			for key, value := range test.set {
				values[key] = value
			}
			if _, err := LoadFrom(testEnvironment(values)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadFrom() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadFromRejectsInvalidMessagingQueueURLs(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":          "postgres://localhost/jungle",
		"AWS_ACCESS_KEY_ID":     "test",
		"AWS_SECRET_ACCESS_KEY": "test",
		"AWS_ENDPOINT_URL":      "http://localhost:4566",
		"SQS_QUEUE_URL":         "http://localhost:4566/queue",
	}
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "outbox queue", key: "SQS_OUTPUT_QUEUE_URL"},
		{name: "dead-letter queue", key: "SQS_DLQ_URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := make(map[string]string, len(base)+1)
			for key, value := range base {
				values[key] = value
			}
			values[test.key] = "not-a-url"
			if _, err := LoadFrom(testEnvironment(values)); err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("LoadFrom() error = %v, want %s validation error", err, test.key)
			}
		})
	}
}

func testEnvironment(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
