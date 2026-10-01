package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

func TestMemoryStorage_UniqueViolationAndStatusConflict(t *testing.T) {
	store := memory.New()
	ctx := context.Background()

	fixedTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store.SetClock(domain.FrozenClock{Current: fixedTime})

	store.SeedUser(&domain.User{ID: 1, Email: "test@example.kz"})
	store.SeedOrder(&domain.Order{ID: 10, UserID: 1, AmountMinor: 5000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
	store.SeedOrder(&domain.Order{ID: 20, UserID: 1, AmountMinor: 5000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

	payRepo := store.Payments()
	orderRepo := store.Orders()

	p1 := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    5000,
		Currency:       "KZT",
		IdempotencyKey: "mem-key-1",
		RequestHash:    "h1",
	}
	if err := payRepo.CreatePending(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}

	// 1. Idempotency key duplicate -> *domain.ErrUniqueViolation with "idempotency_key"
	pDupKey := &domain.Payment{
		UserID:         1,
		OrderID:        20,
		AmountMinor:    5000,
		Currency:       "KZT",
		IdempotencyKey: "mem-key-1",
		RequestHash:    "h2",
	}
	errKey := payRepo.CreatePending(ctx, pDupKey)
	var uniqKeyErr *domain.ErrUniqueViolation
	if !errors.As(errKey, &uniqKeyErr) || uniqKeyErr.Constraint != "idempotency_key" {
		t.Fatalf("expected ErrUniqueViolation with 'idempotency_key', got %v", errKey)
	}

	// 2. Order pending duplicate -> *domain.ErrUniqueViolation with "order_pending"
	pDupOrder := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    5000,
		Currency:       "KZT",
		IdempotencyKey: "mem-key-2",
		RequestHash:    "h3",
	}
	errOrder := payRepo.CreatePending(ctx, pDupOrder)
	var uniqOrderErr *domain.ErrUniqueViolation
	if !errors.As(errOrder, &uniqOrderErr) || uniqOrderErr.Constraint != "order_pending" {
		t.Fatalf("expected ErrUniqueViolation with 'order_pending', got %v", errOrder)
	}

	// 3. UpdateSession allowed on pending
	if err := payRepo.UpdateSession(ctx, p1.ID, "ch_1", "https://pay"); err != nil {
		t.Fatalf("update session on pending failed: %v", err)
	}

	// 4. Update status from pending to succeeded
	if err := payRepo.UpdateStatus(ctx, p1.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
		t.Fatalf("update status failed: %v", err)
	}

	// 5. UpdateSession rejected on non-pending -> domain.ErrStatusConflict
	errSessConflict := payRepo.UpdateSession(ctx, p1.ID, "ch_2", "https://pay2")
	if !errors.Is(errSessConflict, domain.ErrStatusConflict) {
		t.Fatalf("expected ErrStatusConflict on non-pending UpdateSession, got %v", errSessConflict)
	}

	// 6. UpdateStatus rejected when status doesn't match -> domain.ErrStatusConflict
	errStatConflict := payRepo.UpdateStatus(ctx, p1.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed)
	if !errors.Is(errStatConflict, domain.ErrStatusConflict) {
		t.Fatalf("expected ErrStatusConflict on status mismatch, got %v", errStatConflict)
	}

	// 7. Order UpdateStatusTx rejected on mismatch -> domain.ErrStatusConflict
	if err := orderRepo.UpdateStatusTx(ctx, nil, 10, domain.OrderStatusUnpaid, domain.OrderStatusPaid); err != nil {
		t.Fatalf("order update failed: %v", err)
	}
	errOrderConflict := orderRepo.UpdateStatusTx(ctx, nil, 10, domain.OrderStatusUnpaid, domain.OrderStatusCanceled)
	if !errors.Is(errOrderConflict, domain.ErrStatusConflict) {
		t.Fatalf("expected ErrStatusConflict on order mismatch, got %v", errOrderConflict)
	}
}
