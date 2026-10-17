package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	AWSRegion         string
	AWSAccessKeyID    string
	AWSSecretKey      string
	AWSEndpointURL    string
	SQSQueueURL       string
	SQSOutputQueueURL string
	SQSDLQURL         string
	OIDCIssuerURL     string
	OIDCAudience      string
	OIDCInternalURL   string
	StartupTimeout    time.Duration
	ShutdownTimeout   time.Duration
}

func Load() (Config, error) {
	return LoadFrom(os.LookupEnv)
}

func LoadFrom(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		HTTPAddr:        valueOrDefault(lookup, "HTTP_ADDR", ":8080"),
		AWSRegion:       valueOrDefault(lookup, "AWS_REGION", "us-east-1"),
		OIDCIssuerURL:   valueOrDefault(lookup, "OIDC_ISSUER_URL", "http://localhost:8081/realms/jungle-local"),
		OIDCAudience:    valueOrDefault(lookup, "OIDC_AUDIENCE", "jungle-api"),
		OIDCInternalURL: valueOrDefault(lookup, "OIDC_INTERNAL_URL", ""),
		StartupTimeout:  10 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	}

	var err error
	if cfg.DatabaseURL, err = required(lookup, "DATABASE_URL"); err != nil {
		return Config{}, err
	}
	if cfg.AWSAccessKeyID, err = required(lookup, "AWS_ACCESS_KEY_ID"); err != nil {
		return Config{}, err
	}
	if cfg.AWSSecretKey, err = required(lookup, "AWS_SECRET_ACCESS_KEY"); err != nil {
		return Config{}, err
	}
	if cfg.AWSEndpointURL, err = required(lookup, "AWS_ENDPOINT_URL"); err != nil {
		return Config{}, err
	}
	if cfg.SQSQueueURL, err = required(lookup, "SQS_QUEUE_URL"); err != nil {
		return Config{}, err
	}
	cfg.SQSOutputQueueURL = valueOrDefault(lookup, "SQS_OUTPUT_QUEUE_URL", "http://localhost:4566/000000000000/wager-events.fifo")
	cfg.SQSDLQURL = valueOrDefault(lookup, "SQS_DLQ_URL", "http://localhost:4566/000000000000/wager-transactions-dlq.fifo")

	if cfg.StartupTimeout, err = durationOrDefault(lookup, "STARTUP_TIMEOUT", cfg.StartupTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationOrDefault(lookup, "SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return fmt.Errorf("HTTP_ADDR must be a host:port address: %w", err)
	}

	databaseURL, err := url.Parse(c.DatabaseURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") || databaseURL.Hostname() == "" || databaseURL.Path == "" || databaseURL.Path == "/" {
		return fmt.Errorf("DATABASE_URL must be a PostgreSQL URL with host and database name")
	}
	if c.AWSRegion == "" {
		return fmt.Errorf("AWS_REGION must not be empty")
	}
	if err := validateHTTPURL("AWS_ENDPOINT_URL", c.AWSEndpointURL); err != nil {
		return err
	}
	if err := validateHTTPURL("SQS_QUEUE_URL", c.SQSQueueURL); err != nil {
		return err
	}
	if err := validateHTTPURL("SQS_OUTPUT_QUEUE_URL", c.SQSOutputQueueURL); err != nil {
		return err
	}
	if err := validateHTTPURL("SQS_DLQ_URL", c.SQSDLQURL); err != nil {
		return err
	}
	if err := validateHTTPURL("OIDC_ISSUER_URL", c.OIDCIssuerURL); err != nil {
		return err
	}
	if strings.TrimSpace(c.OIDCAudience) == "" {
		return fmt.Errorf("OIDC_AUDIENCE must not be empty")
	}
	if c.OIDCInternalURL != "" {
		if err := validateHTTPURL("OIDC_INTERNAL_URL", c.OIDCInternalURL); err != nil {
			return err
		}
	}
	if c.StartupTimeout <= 0 {
		return fmt.Errorf("STARTUP_TIMEOUT must be greater than zero")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be greater than zero")
	}

	return nil
}

func validateHTTPURL(name, value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute HTTP or HTTPS URL", name)
	}
	return nil
}

func required(lookup func(string) (string, bool), name string) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func valueOrDefault(lookup func(string) (string, bool), name, fallback string) string {
	value, ok := lookup(name)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func durationOrDefault(lookup func(string) (string, bool), name string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return duration, nil
}
