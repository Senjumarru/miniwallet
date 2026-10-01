package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/storage/sqlite"
)

func TestSQLite_MigrationsAndPaymentEvents(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite with migrations: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user & order
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'alice@example.kz');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (10, 1, 50000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("failed to seed test data: %v", err)
	}

	payRepo := sqlite.NewPaymentRepository(store.DB())
	eventRepo := sqlite.NewPaymentEventRepository(store.DB())

	p := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    50000,
		Currency:       "KZT",
		IdempotencyKey: "test-idem-sqlite-1",
		RequestHash:    "hash1",
	}

	if err := payRepo.CreatePending(ctx, p); err != nil {
		t.Fatalf("failed to create pending payment: %v", err)
	}

	// Запись события аудита в payment_events
	err = eventRepo.RecordEventTx(ctx, nil, domain.PaymentEvent{
		PaymentID:  p.ID,
		EventType:  "payment.created",
		FromStatus: "",
		ToStatus:   "pending",
		Metadata:   `{"test":true}`,
	})
	if err != nil {
		t.Fatalf("failed to record payment event: %v", err)
	}

	// Чтение событий аудита
	events, err := eventRepo.ListByPaymentID(ctx, p.ID)
	if err != nil {
		t.Fatalf("failed to list payment events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventType != "payment.created" {
		t.Errorf("expected event_type 'payment.created', got '%s'", events[0].EventType)
	}

	// Проверка выборки зависших платежей
	stale, err := payRepo.GetPendingOlderThan(ctx, time.Now().Add(1*time.Hour), 10)
	if err != nil {
		t.Fatalf("failed to query stale pending: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale payment, got %d", len(stale))
	}
}

func TestGetPendingOlderThan_LocationUTC5(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_tz.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user & order
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'tz_user@example.kz');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (10, 1, 10000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}

	payRepo := sqlite.NewPaymentRepository(store.DB())

	// Insert payment created at 12:00:00 UTC
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO payments (
			user_id, order_id, amount_minor, currency, status,
			idempotency_key, request_hash, created_at, updated_at
		) VALUES (
			1, 10, 10000, 'KZT', 'pending',
			'key-tz-1', 'hash1', '2026-10-01T12:00:00.000Z', '2026-10-01T12:00:00.000Z'
		)
	`)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}

	// olderThan is 11:00:00 UTC, but in UTC+5 it is 16:00:00 +05:00.
	// Since 12:00 UTC is NOT older than 11:00 UTC, 0 payments should be returned.
	locUTC5 := time.FixedZone("UTC+5", 5*3600)
	olderThanUTC5 := time.Date(2026, 10, 1, 16, 0, 0, 0, locUTC5) // equivalent to 11:00:00 UTC

	stale, err := payRepo.GetPendingOlderThan(ctx, olderThanUTC5, 10)
	if err != nil {
		t.Fatalf("GetPendingOlderThan failed: %v", err)
	}

	// If formatting fails to use .UTC(), olderThan is formatted as "2026-10-01T16:00:00.000Z",
	// and since "2026-10-01T12:00:00.000Z" < "2026-10-01T16:00:00.000Z", stale will incorrectly have 1 item!
	if len(stale) != 0 {
		t.Fatalf("expected 0 stale payments because 12:00 UTC is after 11:00 UTC, but got %d", len(stale))
	}
}

func TestPaymentRepository_ErrUniqueViolation(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_uniq.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user & orders
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'uniq_user@example.kz');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (10, 1, 10000, 'KZT', 'unpaid');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (20, 1, 20000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}

	payRepo := sqlite.NewPaymentRepository(store.DB())

	p1 := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    10000,
		Currency:       "KZT",
		IdempotencyKey: "key-idem-unique",
		RequestHash:    "hash1",
	}
	if err := payRepo.CreatePending(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}

	// 1. Try to create payment with same idempotency key for user 1 (order 20)
	pDuplicateKey := &domain.Payment{
		UserID:         1,
		OrderID:        20,
		AmountMinor:    20000,
		Currency:       "KZT",
		IdempotencyKey: "key-idem-unique",
		RequestHash:    "hash2",
	}
	errKey := payRepo.CreatePending(ctx, pDuplicateKey)
	var uniqKeyErr *domain.ErrUniqueViolation
	if !errors.As(errKey, &uniqKeyErr) || uniqKeyErr.Constraint != "idempotency_key" {
		t.Fatalf("expected *domain.ErrUniqueViolation with Constraint 'idempotency_key', got %v", errKey)
	}

	// 2. Try to create payment with different idempotency key for same order 10 (which already has pending payment)
	pDuplicateOrder := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    10000,
		Currency:       "KZT",
		IdempotencyKey: "key-different-key",
		RequestHash:    "hash3",
	}
	errOrder := payRepo.CreatePending(ctx, pDuplicateOrder)
	var uniqOrderErr *domain.ErrUniqueViolation
	if !errors.As(errOrder, &uniqOrderErr) || uniqOrderErr.Constraint != "order_pending" {
		t.Fatalf("expected *domain.ErrUniqueViolation with Constraint 'order_pending', got %v", errOrder)
	}
}

func TestPaymentRepository_UpdateStatusConflict(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_conflict.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user & order
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'conflict_user@example.kz');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (10, 1, 10000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}

	payRepo := sqlite.NewPaymentRepository(store.DB())
	orderRepo := sqlite.NewOrderRepository(store.DB())

	p := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    10000,
		Currency:       "KZT",
		IdempotencyKey: "key-conflict-1",
		RequestHash:    "hash1",
	}
	if err := payRepo.CreatePending(ctx, p); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	// Update payment from pending to succeeded
	if err := payRepo.UpdateStatus(ctx, p.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Now try to update again from pending to failed -> must return domain.ErrStatusConflict
	errConflict := payRepo.UpdateStatus(ctx, p.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed)
	if !errors.Is(errConflict, domain.ErrStatusConflict) {
		t.Fatalf("expected domain.ErrStatusConflict, got %v", errConflict)
	}

	// For a non-existent payment, must return domain.ErrPaymentNotFound
	errNotFound := payRepo.UpdateStatus(ctx, 999999, domain.PaymentStatusPending, domain.PaymentStatusFailed)
	if !errors.Is(errNotFound, domain.ErrPaymentNotFound) {
		t.Fatalf("expected domain.ErrPaymentNotFound, got %v", errNotFound)
	}

	// Test order UpdateStatusTx
	err = store.WithinTransaction(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		// Update order from unpaid to paid
		if err := orderRepo.UpdateStatusTx(txCtx, tx, 10, domain.OrderStatusUnpaid, domain.OrderStatusPaid); err != nil {
			return err
		}
		// Try to update again from unpaid to canceled -> must return domain.ErrStatusConflict
		errOrderConflict := orderRepo.UpdateStatusTx(txCtx, tx, 10, domain.OrderStatusUnpaid, domain.OrderStatusCanceled)
		if !errors.Is(errOrderConflict, domain.ErrStatusConflict) {
			t.Fatalf("expected domain.ErrStatusConflict for order, got %v", errOrderConflict)
		}
		// Non-existent order -> ErrOrderNotFound
		errOrderNotFound := orderRepo.UpdateStatusTx(txCtx, tx, 888888, domain.OrderStatusUnpaid, domain.OrderStatusPaid)
		if !errors.Is(errOrderNotFound, domain.ErrOrderNotFound) {
			t.Fatalf("expected domain.ErrOrderNotFound, got %v", errOrderNotFound)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("order status conflict test: %v", err)
	}
}

func TestPaymentRepository_UpdateSessionPendingOnly(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_session.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user & order
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'sess_user@example.kz');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (10, 1, 10000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}

	payRepo := sqlite.NewPaymentRepository(store.DB())

	p := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    10000,
		Currency:       "KZT",
		IdempotencyKey: "key-sess-1",
		RequestHash:    "hash1",
	}
	if err := payRepo.CreatePending(ctx, p); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	// 1. Updating session while status is 'pending' succeeds
	if err := payRepo.UpdateSession(ctx, p.ID, "ch_prov_1", "https://checkout.url/1"); err != nil {
		t.Fatalf("update session pending failed: %v", err)
	}

	// Now move payment to 'succeeded'
	if err := payRepo.UpdateStatus(ctx, p.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
		t.Fatalf("update status to succeeded failed: %v", err)
	}

	// 2. Updating session on non-pending payment must fail with domain.ErrStatusConflict
	errNonPending := payRepo.UpdateSession(ctx, p.ID, "ch_prov_new", "https://checkout.url/new")
	if !errors.Is(errNonPending, domain.ErrStatusConflict) {
		t.Fatalf("expected domain.ErrStatusConflict when updating non-pending payment session, got %v", errNonPending)
	}

	// 3. Updating non-existent payment must return domain.ErrPaymentNotFound
	errNotFound := payRepo.UpdateSession(ctx, 999999, "ch_prov_x", "https://checkout.url/x")
	if !errors.Is(errNotFound, domain.ErrPaymentNotFound) {
		t.Fatalf("expected domain.ErrPaymentNotFound, got %v", errNotFound)
	}
}

func TestMigrations_PartialUniqueIndexSucceededAndSecurityEvents(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_mig.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user & order
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'mig_user@example.kz');
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (10, 1, 10000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}

	// Insert first succeeded payment for order 10
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO payments (user_id, order_id, amount_minor, currency, status, idempotency_key, request_hash)
		VALUES (1, 10, 10000, 'KZT', 'succeeded', 'k-succ-1', 'h1')
	`)
	if err != nil {
		t.Fatalf("insert first succeeded payment: %v", err)
	}

	// Attempt to insert second succeeded payment for same order 10 -> MUST fail due to partial unique index!
	_, errSecond := store.DB().ExecContext(ctx, `
		INSERT INTO payments (user_id, order_id, amount_minor, currency, status, idempotency_key, request_hash)
		VALUES (1, 10, 10000, 'KZT', 'succeeded', 'k-succ-2', 'h2')
	`)
	if errSecond == nil {
		t.Fatalf("expected error due to idx_payments_order_single_succeeded, but insert succeeded!")
	}

	// Verify security_events table exists and accepts NULL payment_id
	_, errSec := store.DB().ExecContext(ctx, `
		INSERT INTO security_events (payment_id, event_type, metadata)
		VALUES (NULL, 'security.unauthorized_attempt', '{"ip":"127.0.0.1"}')
	`)
	if errSec != nil {
		t.Fatalf("failed to insert security event with NULL payment_id: %v", errSec)
	}
}

func TestWebhookEventRepository_OnConflictDoNothing(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_webhook.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	webhookRepo := sqlite.NewWebhookEventRepository(store.DB())

	// First insert: must succeed with isDuplicate = false
	isDup, err := webhookRepo.RecordEventTx(ctx, nil, "evt_test_123", "payment.succeeded", []byte(`{"id":123}`))
	if err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	if isDup {
		t.Fatalf("expected isDuplicate=false on first insert, got true")
	}

	// Second insert with same event_id: must return isDuplicate = true and no error
	isDup2, err2 := webhookRepo.RecordEventTx(ctx, nil, "evt_test_123", "payment.succeeded", []byte(`{"id":123}`))
	if err2 != nil {
		t.Fatalf("second insert failed: %v", err2)
	}
	if !isDup2 {
		t.Fatalf("expected isDuplicate=true on duplicate insert, got false")
	}
}

func TestSQLite_Concurrent50WritingTransactions(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_50_writes.db")
	migrationsDir := filepath.Join("..", "..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed user
	_, err = store.DB().ExecContext(ctx, `INSERT INTO users (id, email) VALUES (1, 'concurrent@example.kz');`)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	startBarrier := make(chan struct{})

	for i := 1; i <= goroutines; i++ {
		wg.Add(1)
		orderID := int64(i)
		go func(id int64) {
			defer wg.Done()
			<-startBarrier

			// Each goroutine executes a write transaction with BEGIN IMMEDIATE
			txErr := store.WithinTransaction(ctx, func(txCtx context.Context, tx *sql.Tx) error {
				_, execErr := tx.ExecContext(txCtx, `
					INSERT INTO orders (id, user_id, amount_minor, currency, status)
					VALUES (?, 1, ?, 'KZT', 'unpaid')`, id, id*100)
				return execErr
			})
			if txErr != nil {
				errCh <- fmt.Errorf("goroutine %d: %w", id, txErr)
			}
		}(orderID)
	}

	// Release all 50 goroutines simultaneously
	close(startBarrier)
	wg.Wait()
	close(errCh)

	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}

	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("write transaction error: %v", e)
		}
		t.Fatalf("50 parallel write transactions failed with %d errors (e.g. database is locked)", len(errs))
	}

	// Verify that all 50 rows were written
	var count int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM orders").Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != goroutines {
		t.Fatalf("expected %d orders, found %d", goroutines, count)
	}
}
