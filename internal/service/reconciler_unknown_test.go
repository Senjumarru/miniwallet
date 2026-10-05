package service_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/metrics"
	"github.com/Senjumarru/miniwallet/internal/provider"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

func TestReconciler_PaymentUnknown_RemainsPendingAndIncrementsMetric(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov := &fakeProvider{
		statusCheckErr: provider.ErrProviderPaymentUnknown,
	}

	reg := prometheus.NewRegistry()
	promMetrics := metrics.NewMetrics(reg)

	cfg := service.ReconcilerConfig{
		TTL:      15 * time.Minute,
		Interval: 1 * time.Minute,
		Batch:    10,
	}

	r := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		cfg,
	)
	r.SetMetrics(promMetrics)

	ctx := context.Background()

	// Seed user and order
	store.SeedUser(&domain.User{ID: 1, Email: "u@kz", IsActive: true})
	store.SeedOrder(&domain.Order{ID: 10, UserID: 1, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

	// Seed pending payment created 30 minutes ago (> TTL 15m)
	oldTime := time.Now().UTC().Add(-30 * time.Minute)
	p := &domain.Payment{
		UserID:            1,
		OrderID:           10,
		AmountMinor:       50000,
		Currency:          "KZT",
		Status:            domain.PaymentStatusPending,
		IdempotencyKey:    "test-unknown-404-idem",
		RequestHash:       "hash",
		ProviderPaymentID: "ch_unknown_404",
		CreatedAt:         oldTime,
		UpdatedAt:         oldTime,
	}
	if err := store.Payments().CreatePending(ctx, p); err != nil {
		t.Fatalf("failed to create pending payment: %v", err)
	}

	// Run ReconcileOnce
	_, err := r.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce returned unexpected error: %v", err)
	}

	// 1. Payment must REMAIN in pending status (Invariant 5)
	checkP, err := store.Payments().GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("failed to fetch payment: %v", err)
	}
	if checkP.Status != domain.PaymentStatusPending {
		t.Fatalf("payment status must remain pending on 404, got %s", checkP.Status)
	}

	// 2. Metric ReconcilerUnknownPaymentsTotal must be incremented to 1
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "miniwallet_reconciler_unknown_payments_total 1") {
		t.Fatalf("expected ReconcilerUnknownPaymentsTotal metric to be 1, got body:\n%s", body)
	}
}
