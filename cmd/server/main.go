package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Senjumarru/miniwallet/internal/config"
	"github.com/Senjumarru/miniwallet/internal/httpapi"
	"github.com/Senjumarru/miniwallet/internal/metrics"
	"github.com/Senjumarru/miniwallet/internal/provider"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/sqlite"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal application error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if cfg.Dev {
		logger.Warn("DEV MODE: insecure defaults, never use in production")
	}

	store, err := sqlite.Open(cfg.DBPath, "migrations")
	if err != nil {
		return err
	}
	defer store.Close()
	logger.Info("database storage initialized", slog.String("db_path", cfg.DBPath))

	userRepo := sqlite.NewUserRepository(store.DB())
	orderRepo := sqlite.NewOrderRepository(store.DB())
	paymentRepo := sqlite.NewPaymentRepository(store.DB())
	webhookRepo := sqlite.NewWebhookEventRepository(store.DB())
	paymentEventRepo := sqlite.NewPaymentEventRepository(store.DB())
	securityEventRepo := sqlite.NewSecurityEventRepository(store.DB())

	providerClient := provider.NewClient(
		cfg.ProviderBaseURL,
		&http.Client{Timeout: cfg.ProviderTimeout},
		logger,
		cfg.ProviderTimeout,
		cfg.MaxRetryAttempts,
		cfg.InitialRetryDelay,
		cfg.MaxRetryDelay,
	)

	paymentSvc := service.NewPaymentService(
		userRepo,
		orderRepo,
		paymentRepo,
		webhookRepo,
		paymentEventRepo,
		securityEventRepo,
		store,
		providerClient,
		logger,
		cfg.WebhookSecret,
	)

	// Graceful shutdown context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Запуск фонового воркера сверки зависших pending-платежей с TTL
	reconciler := service.NewReconciler(
		paymentRepo,
		orderRepo,
		paymentEventRepo,
		store,
		providerClient,
		logger,
		service.ReconcilerConfig{
			TTL:      15 * time.Minute,
			Interval: 1 * time.Minute,
			Batch:    50,
		},
	)

	promMetrics := metrics.NewMetrics(nil)
	providerClient.SetMetrics(promMetrics)
	paymentSvc.SetMetrics(promMetrics)
	reconciler.SetMetrics(promMetrics)

	go reconciler.Start(ctx)

	handler := httpapi.NewHandler(paymentSvc, logger, cfg.JWTSecret,
		httpapi.WithReadyChecker(store.DB().PingContext),
	)

	// 5.3: http.Server с обязательными таймаутами и лимитами заголовков
	srv := httpapi.NewHTTPServer(net.JoinHostPort("localhost", cfg.Port), handler)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", slog.String("addr", srv.Addr))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received: waiting for active requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
		} else {
			logger.Info("graceful shutdown completed")
		}
	}

	return nil
}
