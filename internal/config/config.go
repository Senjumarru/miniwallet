package config

import (
	"errors"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port              string
	DBPath            string
	WebhookSecret     string
	JWTSecret         string
	ProviderBaseURL   string
	ProviderTimeout   time.Duration
	MaxRetryAttempts  int
	InitialRetryDelay time.Duration
	MaxRetryDelay     time.Duration
	LogLevel          string
}

func Load() (*Config, error) {
	jwtSec := getEnv("JWT_SECRET", "dev-jwt-secret-change-me")
	whSec := getEnv("WEBHOOK_SECRET", "dev-webhook-secret-change-me")

	// Отказ запуска без секретов: явная пустая строка либо дефолтные dev-секреты в production
	if val, ok := os.LookupEnv("JWT_SECRET"); ok && val == "" {
		return nil, errors.New("JWT_SECRET cannot be empty")
	}
	if val, ok := os.LookupEnv("WEBHOOK_SECRET"); ok && val == "" {
		return nil, errors.New("WEBHOOK_SECRET cannot be empty")
	}
	if os.Getenv("APP_ENV") == "production" || os.Getenv("REQUIRE_SECRETS") == "true" {
		if os.Getenv("JWT_SECRET") == "" || os.Getenv("JWT_SECRET") == "dev-jwt-secret-change-me" {
			return nil, errors.New("JWT_SECRET is required and must not use dev default")
		}
		if os.Getenv("WEBHOOK_SECRET") == "" || os.Getenv("WEBHOOK_SECRET") == "dev-webhook-secret-change-me" {
			return nil, errors.New("WEBHOOK_SECRET is required and must not use dev default")
		}
	}

	cfg := &Config{
		Port:              getEnv("PORT", "8080"),
		DBPath:            getEnv("DB_PATH", "wallet.db"),
		WebhookSecret:     whSec,
		JWTSecret:         jwtSec,
		ProviderBaseURL:   getEnv("PROVIDER_BASE_URL", "http://localhost:8081"),
		ProviderTimeout:   5 * time.Second,
		MaxRetryAttempts:  4,
		InitialRetryDelay: 100 * time.Millisecond,
		MaxRetryDelay:     2 * time.Second,
		LogLevel:          getEnv("LOG_LEVEL", "info"),
	}

	if t := os.Getenv("PROVIDER_TIMEOUT_MS"); t != "" {
		if ms, err := strconv.Atoi(t); err == nil && ms > 0 {
			cfg.ProviderTimeout = time.Duration(ms) * time.Millisecond
		}
	}

	if a := os.Getenv("MAX_RETRY_ATTEMPTS"); a != "" {
		if val, err := strconv.Atoi(a); err == nil && val > 0 {
			cfg.MaxRetryAttempts = val
		}
	}

	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
