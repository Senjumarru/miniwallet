package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/config"
)

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"JWT_SECRET", "WEBHOOK_SECRET", "APP_ENV", "REQUIRE_SECRETS",
		"PORT", "DB_PATH", "PROVIDER_BASE_URL", "PROVIDER_TIMEOUT_MS",
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

func TestLoad_DefaultDevConfig(t *testing.T) {
	clearEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected nil err on default dev config, got: %v", err)
	}
	if cfg.JWTSecret != "dev-jwt-secret-change-me" {
		t.Errorf("expected dev default JWTSecret, got %q", cfg.JWTSecret)
	}
	if cfg.WebhookSecret != "dev-webhook-secret-change-me" {
		t.Errorf("expected dev default WebhookSecret, got %q", cfg.WebhookSecret)
	}
	if cfg.Port != "8080" {
		t.Errorf("expected port 8080, got %q", cfg.Port)
	}
}

func TestLoad_EmptySecretRejection(t *testing.T) {
	t.Run("empty JWT_SECRET rejected", func(t *testing.T) {
		clearEnv(t)
		os.Setenv("JWT_SECRET", "")

		cfg, err := config.Load()
		if err == nil {
			t.Fatal("expected error when JWT_SECRET is empty string, got nil")
		}
		if cfg != nil {
			t.Fatalf("expected nil config on error, got %+v", cfg)
		}
	})

	t.Run("empty WEBHOOK_SECRET rejected", func(t *testing.T) {
		clearEnv(t)
		os.Setenv("WEBHOOK_SECRET", "")

		cfg, err := config.Load()
		if err == nil {
			t.Fatal("expected error when WEBHOOK_SECRET is empty string, got nil")
		}
		if cfg != nil {
			t.Fatalf("expected nil config on error, got %+v", cfg)
		}
	})
}

func TestLoad_ProductionRequiresRealSecrets(t *testing.T) {
	tests := []struct {
		name        string
		envKey      string
		envVal      string
		jwtSecret   string
		whSecret    string
		expectErr   bool
		errContains string
	}{
		{
			name:        "production with default jwt secret fails",
			envKey:      "APP_ENV",
			envVal:      "production",
			jwtSecret:   "dev-jwt-secret-change-me",
			whSecret:    "prod-wh-secret-123",
			expectErr:   true,
			errContains: "JWT_SECRET is required",
		},
		{
			name:        "production with default webhook secret fails",
			envKey:      "APP_ENV",
			envVal:      "production",
			jwtSecret:   "prod-jwt-secret-123",
			whSecret:    "dev-webhook-secret-change-me",
			expectErr:   true,
			errContains: "WEBHOOK_SECRET is required",
		},
		{
			name:        "require_secrets flag with default secret fails",
			envKey:      "REQUIRE_SECRETS",
			envVal:      "true",
			jwtSecret:   "dev-jwt-secret-change-me",
			whSecret:    "prod-wh-secret-123",
			expectErr:   true,
			errContains: "JWT_SECRET is required",
		},
		{
			name:      "production with valid custom secrets succeeds",
			envKey:    "APP_ENV",
			envVal:    "production",
			jwtSecret: "strong-secret-prod-1",
			whSecret:  "strong-secret-prod-2",
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			os.Setenv(tc.envKey, tc.envVal)
			os.Setenv("JWT_SECRET", tc.jwtSecret)
			os.Setenv("WEBHOOK_SECRET", tc.whSecret)

			cfg, err := config.Load()
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error for case %q, got nil", tc.name)
				}
			} else {
				if err != nil {
					t.Fatalf("expected success for case %q, got err: %v", tc.name, err)
				}
				if cfg.JWTSecret != tc.jwtSecret || cfg.WebhookSecret != tc.whSecret {
					t.Fatalf("config secrets mismatch: %+v", cfg)
				}
			}
		})
	}
}

func TestLoad_CustomEnvVariables(t *testing.T) {
	clearEnv(t)
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
