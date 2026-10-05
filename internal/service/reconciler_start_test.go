package service_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

type trackingPaymentRepo struct {
	service.PaymentRepository
	getPendingCalls atomic.Int32
}

func (t *trackingPaymentRepo) GetPendingOlderThan(ctx context.Context, olderThan time.Time, limit int) ([]*domain.Payment, error) {
	t.getPendingCalls.Add(1)
	return t.PaymentRepository.GetPendingOlderThan(ctx, olderThan, limit)
}

func TestReconciler_Start_PeriodicityCancellationGoleak(t *testing.T) {
	defer goleak.VerifyNone(t)

	store := memory.New()
	trackingRepo := &trackingPaymentRepo{PaymentRepository: store.Payments()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov := &fakeProvider{}

	interval := 20 * time.Millisecond
	cfg := service.ReconcilerConfig{
		TTL:      50 * time.Millisecond,
		Interval: interval,
		Batch:    10,
	}

	r := service.NewReconciler(
		trackingRepo,
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		cfg,
	)

	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	done := make(chan struct{})

	go func() {
		close(started)
		r.Start(ctx)
		close(done)
	}()

	<-started

	// Allow multiple ticks to occur (e.g. 20ms * 4 = 80ms)
	time.Sleep(75 * time.Millisecond)

	// Stop worker via context cancellation
	cancel()

	select {
	case <-done:
		// Reconciler gracefully stopped on context cancellation
	case <-time.After(1 * time.Second):
		t.Fatal("reconciler.Start did not stop within timeout after ctx.Done()")
	}

	calls := trackingRepo.getPendingCalls.Load()
	if calls < 2 {
		t.Fatalf("expected at least 2 periodic reconciliation ticks, got %d", calls)
	}
}
