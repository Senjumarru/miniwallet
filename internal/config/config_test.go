package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/config"
)

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"JWT_SECRET", "WEBHOOK_SECRET", "WEBHOOK_SECRET_OLD", "APP_ENV", "REQUIRE_SECRETS",
		"HOST", "PORT", "DB_PATH", "PROVIDER_BASE_URL", "PROVIDER_TIMEOUT_MS",
		"MAX_RETRY_ATTEMPTS", "LOG_LEVEL",
	}
	for _, k := range keys {
		oldVal, had := os.LookupEnv(k)
		os.Unsetenv(k)
		if had {
			t.Cleanup(func() {
				os.Setenv(k, oldVal)
			})
		} else {
			t.Cleanup(func() {
				os.Unsetenv(k)
			})
		}
	}
}

// Invariant 7: Секреты без значений по умолчанию: отказ запуска при пустых, коротких
// (<32 байт), dev-значениях или одинаковых секретах. Небезопасный режим только при явном APP_ENV=dev.

func TestLoad_FailClosedByDefault(t *testing.T) {
	clearEnv(t)

	// Без APP_ENV=dev и без секретов config.Load ОБЯЗАН падать (fail-closed)
	cfg, err := config.Load()
	if err == nil {
		t.Fatalf("expected error by default when secrets are missing (fail-closed), got config: %+v", cfg)
	}
}

func TestLoad_ShortSecretRejected(t *testing.T) {
	clearEnv(t)
	// 31 байт (< 32 байт)
	shortKey := "1234567890123456789012345678901"
	validKey := "abcdefghijklmnopqrstuvwxyz1234567890_strong_key_1"

	t.Run("short JWT_SECRET rejected", func(t *testing.T) {
		clearEnv(t)
		os.Setenv("JWT_SECRET", shortKey)
		os.Setenv("WEBHOOK_SECRET", validKey)
		_, err := config.Load()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "short") {
			t.Fatalf("expected error about short JWT_SECRET, got: %v", err)
		}
	})

	t.Run("short WEBHOOK_SECRET rejected", func(t *testing.T) {
		clearEnv(t)
		os.Setenv("JWT_SECRET", validKey)
		os.Setenv("WEBHOOK_SECRET", shortKey)
		_, err := config.Load()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "short") {
			t.Fatalf("expected error about short WEBHOOK_SECRET, got: %v", err)
		}
	})
}

func TestLoad_IdenticalSecretsRejected(t *testing.T) {
	clearEnv(t)
	sameKey := "super_long_secret_key_used_for_both_jwt_and_webhook_123"
	os.Setenv("JWT_SECRET", sameKey)
	os.Setenv("WEBHOOK_SECRET", sameKey)

	_, err := config.Load()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "identical") {
		t.Fatalf("expected error about identical secrets, got: %v", err)
	}
}

func TestLoad_DevValuesRejectedInNonDev(t *testing.T) {
	clearEnv(t)
	validKey := "a_very_secure_and_random_string_of_bytes_for_production_use_123"

	devKeys := []string{
		"dev-jwt-secret-change-me",
		"dev-webhook-secret-change-me",
		"dev-secret-change-me",
		"dev-jwt-secret-change-me-and-more-text",
		"this-is-a-changeme-secret-that-is-long-enough",
		"my-insecure-password-must-be-rejected-even-if-long",
		"an-example-secret-that-exceeds-thirty-two-bytes",
	}

	for _, devKey := range devKeys {
		t.Run("jwt_"+devKey, func(t *testing.T) {
			clearEnv(t)
			os.Setenv("JWT_SECRET", devKey)
			os.Setenv("WEBHOOK_SECRET", validKey)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected error for dev secret %q in non-dev mode, got nil", devKey)
			}
		})

		t.Run("webhook_"+devKey, func(t *testing.T) {
			clearEnv(t)
			os.Setenv("JWT_SECRET", validKey)
			os.Setenv("WEBHOOK_SECRET", devKey)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected error for dev secret %q in non-dev mode, got nil", devKey)
			}
		})
	}
}

func TestLoad_FewerThan8DistinctCharsRejected(t *testing.T) {
	clearEnv(t)
	validKey := "a_very_secure_and_random_string_of_bytes_for_production_use_123"

	lowEntropyKeys := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",                                 // 1 distinct
		"abababababababababababababababab",                                 // 2 distinct
		"12345671234567123456712345671234",                                 // 7 distinct
		"----------------------------------------------------------------", // 1 distinct
	}

	for _, lowKey := range lowEntropyKeys {
		t.Run("low_entropy_"+lowKey[:8], func(t *testing.T) {
			clearEnv(t)
			os.Setenv("JWT_SECRET", lowKey)
			os.Setenv("WEBHOOK_SECRET", validKey)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected error for secret with < 8 distinct chars %q, got nil", lowKey)
			}
		})
	}
}

func TestLoad_RandomHexAndBase64SecretsPass(t *testing.T) {
	clearEnv(t)

	// Настоящий 64-символьный hex (32 байта энтропии)
	randomHexJWT := "d4e5f601a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9"
	// Настоящий base64 секрет (44 символа, 32 байта)
	randomBase64WH := "c2VjdXJlX3JhbmRvbV9iYXNlNjRfc2VjcmV0X3ZhbHVlXzEyMw=="

	os.Setenv("JWT_SECRET", randomHexJWT)
	os.Setenv("WEBHOOK_SECRET", randomBase64WH)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected random hex and base64 secrets to pass, got: %v", err)
	}
	if cfg.Dev {
		t.Errorf("expected Dev to be false in non-dev mode, got true")
	}
	if cfg.JWTSecret != randomHexJWT || cfg.WebhookSecret != randomBase64WH {
		t.Fatalf("loaded secrets mismatch: %+v", cfg)
	}
}

func TestLoad_ExplicitDevModeAllowed(t *testing.T) {
	clearEnv(t)
	os.Setenv("APP_ENV", "dev")

	// В явном APP_ENV=dev запуск без секретов должен быть разрешён со значениями по умолчанию
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected success in APP_ENV=dev mode, got error: %v", err)
	}
	if !cfg.Dev {
		t.Errorf("expected Dev=true in APP_ENV=dev mode, got false")
	}
	if cfg.JWTSecret == "" || cfg.WebhookSecret == "" {
		t.Fatal("expected dev secrets to be populated in dev mode")
	}
}

func TestLoad_ValidProductionConfig(t *testing.T) {
	clearEnv(t)
	jwtKey := "strong_production_jwt_secret_key_1234567890_min_32"
	whKey := "strong_production_webhook_secret_key_1234567890_min_32"

	os.Setenv("JWT_SECRET", jwtKey)
	os.Setenv("WEBHOOK_SECRET", whKey)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected success with valid strong secrets, got: %v", err)
	}
	if cfg.Dev {
		t.Errorf("expected Dev=false in production config, got true")
	}
	if cfg.JWTSecret != jwtKey || cfg.WebhookSecret != whKey {
		t.Fatalf("secrets not matching: %+v", cfg)
	}
}

func TestLoad_CustomEnvVariables(t *testing.T) {
	clearEnv(t)
	os.Setenv("APP_ENV", "dev")
	os.Setenv("PORT", "9090")
	os.Setenv("DB_PATH", "test.db")
	os.Setenv("PROVIDER_BASE_URL", "http://provider:9999")
	os.Setenv("PROVIDER_TIMEOUT_MS", "3000")
	os.Setenv("MAX_RETRY_ATTEMPTS", "5")
	os.Setenv("LOG_LEVEL", "debug")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "9090" {
		t.Errorf("expected port 9090, got %s", cfg.Port)
	}
	if cfg.DBPath != "test.db" {
		t.Errorf("expected DBPath test.db, got %s", cfg.DBPath)
	}
	if cfg.ProviderBaseURL != "http://provider:9999" {
		t.Errorf("expected URL http://provider:9999, got %s", cfg.ProviderBaseURL)
	}
	if cfg.ProviderTimeout != 3*time.Second {
		t.Errorf("expected timeout 3s, got %v", cfg.ProviderTimeout)
	}
	if cfg.MaxRetryAttempts != 5 {
		t.Errorf("expected 5 retries, got %d", cfg.MaxRetryAttempts)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected debug, got %s", cfg.LogLevel)
	}
}

func TestLoad_Host(t *testing.T) {
	clearEnv(t)
	os.Setenv("APP_ENV", "dev")

	t.Run("default host is localhost", func(t *testing.T) {
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Host != "localhost" {
			t.Errorf("expected default host localhost, got %s", cfg.Host)
		}
	})

	t.Run("custom host from env", func(t *testing.T) {
		os.Setenv("HOST", "0.0.0.0")
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Host != "0.0.0.0" {
			t.Errorf("expected host 0.0.0.0, got %s", cfg.Host)
		}
	})
}

func TestLoad_WebhookSecretOld(t *testing.T) {
	clearEnv(t)
	jwtKey := "strong_production_jwt_secret_key_1234567890_min_32"
	whKey := "strong_production_webhook_secret_key_1234567890_min_32"
	validOldKey := "strong_old_webhook_secret_key_1234567890_min_32_old"

	os.Setenv("JWT_SECRET", jwtKey)
	os.Setenv("WEBHOOK_SECRET", whKey)

	t.Run("valid WEBHOOK_SECRET_OLD loads correctly", func(t *testing.T) {
		os.Setenv("WEBHOOK_SECRET_OLD", validOldKey)
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.WebhookSecretOld != validOldKey {
			t.Errorf("expected old webhook secret %s, got %s", validOldKey, cfg.WebhookSecretOld)
		}
	})

	t.Run("short WEBHOOK_SECRET_OLD rejected in non-dev", func(t *testing.T) {
		os.Setenv("WEBHOOK_SECRET_OLD", "too_short_old_key_123")
		_, err := config.Load()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "short") {
			t.Fatalf("expected error about short WEBHOOK_SECRET_OLD, got: %v", err)
		}
	})

	t.Run("identical to WEBHOOK_SECRET rejected", func(t *testing.T) {
		os.Setenv("WEBHOOK_SECRET_OLD", whKey)
		_, err := config.Load()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "identical") {
			t.Fatalf("expected error about identical webhook secrets, got: %v", err)
		}
	})

	t.Run("dev value rejected in non-dev", func(t *testing.T) {
		os.Setenv("WEBHOOK_SECRET_OLD", "dev-webhook-secret-change-me-old-prod")
		_, err := config.Load()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "dev-value") {
			t.Fatalf("expected error about dev value in non-dev, got: %v", err)
		}
	})
}
