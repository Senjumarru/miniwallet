package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host              string
	Port              string
	DBPath            string
	WebhookSecret     string
	WebhookSecretOld  string
	JWTSecret         string
	ProviderBaseURL   string
	ProviderTimeout   time.Duration
	MaxRetryAttempts  int
	InitialRetryDelay time.Duration
	MaxRetryDelay     time.Duration
	LogLevel          string
	Dev               bool
}

func Load() (*Config, error) {
	appEnv := os.Getenv("APP_ENV")
	isDev := appEnv == "dev"

	var jwtSec, whSec, whSecOld string

	if isDev {
		jwtSec = getEnv("JWT_SECRET", "dev-jwt-secret-change-me-local-dev-only")
		whSec = getEnv("WEBHOOK_SECRET", "dev-webhook-secret-change-me-local-dev-only")
		whSecOld = os.Getenv("WEBHOOK_SECRET_OLD")
	} else {
		// Инвариант 7: Секреты без значений по умолчанию: отказ запуска при пустых, коротких
		// (<32 байт), dev-значениях или одинаковых секретах. Небезопасный режим только при явном APP_ENV=dev.
		jwtSec = os.Getenv("JWT_SECRET")
		if jwtSec == "" {
			return nil, errors.New("JWT_SECRET is required and cannot be empty (fail-closed, set APP_ENV=dev for local development)")
		}
		if len([]byte(jwtSec)) < 32 {
			return nil, fmt.Errorf("JWT_SECRET is too short: must be at least 32 bytes (got %d)", len([]byte(jwtSec)))
		}
		if isDevSecret(jwtSec) {
			return nil, errors.New("JWT_SECRET must not use insecure dev-value in non-dev environment")
		}

		whSec = os.Getenv("WEBHOOK_SECRET")
		if whSec == "" {
			return nil, errors.New("WEBHOOK_SECRET is required and cannot be empty (fail-closed, set APP_ENV=dev for local development)")
		}
		if len([]byte(whSec)) < 32 {
			return nil, fmt.Errorf("WEBHOOK_SECRET is too short: must be at least 32 bytes (got %d)", len([]byte(whSec)))
		}
		if isDevSecret(whSec) {
			return nil, errors.New("WEBHOOK_SECRET must not use insecure dev-value in non-dev environment")
		}

		if jwtSec == whSec {
			return nil, errors.New("JWT_SECRET and WEBHOOK_SECRET must not be identical")
		}

		whSecOld = os.Getenv("WEBHOOK_SECRET_OLD")
		if whSecOld != "" {
			if len([]byte(whSecOld)) < 32 {
				return nil, fmt.Errorf("WEBHOOK_SECRET_OLD is too short: must be at least 32 bytes (got %d)", len([]byte(whSecOld)))
			}
			if isDevSecret(whSecOld) {
				return nil, errors.New("WEBHOOK_SECRET_OLD must not use insecure dev-value in non-dev environment")
			}
			if whSecOld == whSec {
				return nil, errors.New("WEBHOOK_SECRET_OLD and WEBHOOK_SECRET must not be identical")
			}
			if whSecOld == jwtSec {
				return nil, errors.New("WEBHOOK_SECRET_OLD and JWT_SECRET must not be identical")
			}
		}
	}

	cfg := &Config{
		Host:              getEnv("HOST", "localhost"),
		Port:              getEnv("PORT", "8080"),
		DBPath:            getEnv("DB_PATH", "wallet.db"),
		WebhookSecret:     whSec,
		WebhookSecretOld:  whSecOld,
		JWTSecret:         jwtSec,
		ProviderBaseURL:   getEnv("PROVIDER_BASE_URL", "http://localhost:8081"),
		ProviderTimeout:   5 * time.Second,
		MaxRetryAttempts:  4,
		InitialRetryDelay: 100 * time.Millisecond,
		MaxRetryDelay:     2 * time.Second,
		LogLevel:          getEnv("LOG_LEVEL", "info"),
		Dev:               isDev,
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

func isDevSecret(secret string) bool {
	lower := strings.ToLower(secret)
	markers := []string{
		"change-me",
		"changeme",
		"dev-secret",
		"dev-jwt-secret",
		"dev-webhook-secret",
		"password",
		"example",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}

	// Отказ для секретов менее чем с 8 разными символами (низкая энтропия)
	distinct := make(map[rune]struct{}, 8)
	for _, r := range secret {
		distinct[r] = struct{}{}
		if len(distinct) >= 8 {
			return false
		}
	}
	return true
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
