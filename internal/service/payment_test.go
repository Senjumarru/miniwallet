package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/provider"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
	"github.com/Senjumarru/miniwallet/internal/storage/sqlite"
)

type fakeProvider struct {
	calls              atomic.Int64
	failCheckout       bool
	checkoutErr        error
	statusToReturn     domain.PaymentStatus
	statusCheckCalls   atomic.Int64
	statusCheckErr     error
	statusByID         map[string]domain.PaymentStatus
	errByID            map[string]error
	lastIdempotencyKey string
	lastStatusCheckID  string
	mu                 sync.Mutex
}

func (f *fakeProvider) CreateCheckoutSession(ctx context.Context, in provider.CreateCheckoutInput) (provider.CheckoutSession, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastIdempotencyKey = in.IdempotencyKey
	f.mu.Unlock()

	if f.failCheckout {
		if f.checkoutErr != nil {
			return provider.CheckoutSession{}, f.checkoutErr
		}
		return provider.CheckoutSession{}, errors.New("simulated provider gateway timeout")
	}
	return provider.CheckoutSession{
		ProviderPaymentID: fmt.Sprintf("ch_prov_%d", in.PaymentID),
		CheckoutURL:       fmt.Sprintf("https://checkout.fake/pay/%d", in.PaymentID),
	}, nil
}

func (f *fakeProvider) GetPaymentStatus(ctx context.Context, providerPaymentID string) (domain.PaymentStatus, error) {
	f.statusCheckCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStatusCheckID = providerPaymentID

	if f.errByID != nil {
		if err, ok := f.errByID[providerPaymentID]; ok && err != nil {
			return "", err
		}
	}
	if f.statusCheckErr != nil {
		return "", f.statusCheckErr
	}
	if f.statusByID != nil {
		if st, ok := f.statusByID[providerPaymentID]; ok {
			return st, nil
		}
	}
	if f.statusToReturn != "" {
		return f.statusToReturn, nil
	}
	return domain.PaymentStatusPending, nil
}

func setupTestService(t *testing.T) (*service.PaymentService, *memory.Storage, *fakeProvider, string) {
	t.Helper()
	store := memory.New()
	prov := &fakeProvider{}
	secret := "super-secure-webhook-secret-key-12345"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := service.NewPaymentService(
		store.Users(),
		store.Orders(),
		store.Payments(),
		store.Webhooks(),
		store.PaymentEvents(),
		store.SecurityEvents(),
		store,
		prov,
		logger,
		secret,
	)

	store.SeedUser(&domain.User{
		ID:        1,
		Email:     "alice@example.kz",
		IsActive:  true,
		IsBlocked: false,
	})
	store.SeedUser(&domain.User{
		ID:        2,
		Email:     "blocked@example.kz",
		IsActive:  true,
		IsBlocked: true,
	})

	store.SeedOrder(&domain.Order{
		ID:          10,
		UserID:      1,
		AmountMinor: 50000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})
	store.SeedOrder(&domain.Order{
		ID:          11,
		UserID:      1,
		AmountMinor: 30000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	return svc, store, prov, secret
}

func setupTestServiceSQLite(t *testing.T) (*service.PaymentService, *sqlite.Storage, *fakeProvider, string) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	migrationsDir := filepath.Join("..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Logf("cleanup store close error: %v", closeErr)
		}
	})

	ctx := context.Background()
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO users (id, email) VALUES (1, 'alice@example.kz'), (2, 'blocked@example.kz');
		UPDATE users SET is_blocked = 1 WHERE id = 2;
		INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES 
			(10, 1, 50000, 'KZT', 'unpaid'),
			(11, 1, 30000, 'KZT', 'unpaid');
	`)
	if err != nil {
		t.Fatalf("failed to seed sqlite test data: %v", err)
	}

	prov := &fakeProvider{}
	secret := "super-secure-webhook-secret-key-12345"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := service.NewPaymentService(
		sqlite.NewUserRepository(store.DB()),
		sqlite.NewOrderRepository(store.DB()),
		sqlite.NewPaymentRepository(store.DB()),
		sqlite.NewWebhookEventRepository(store.DB()),
		sqlite.NewPaymentEventRepository(store.DB()),
		sqlite.NewSecurityEventRepository(store.DB()),
		store,
		prov,
		logger,
		secret,
	)

	return svc, store, prov, secret
}

func signWebhook(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", timestamp)))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestPaymentService_Idempotency(t *testing.T) {
	svc, _, prov, _ := setupTestService(t)
	ctx := context.Background()

	key := "idem-test-key-12345678"

	// 1. Первый вызов — создание платежа
	res1, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("first CreatePayment failed: %v", err)
	}
	if res1.IsReplay {
		t.Fatal("first call must not be replay")
	}
	if prov.calls.Load() != 1 {
		t.Fatalf("expected 1 provider call, got %d", prov.calls.Load())
	}

	// 2. Повтор с тем же ключом и тем же заказом — возврат прежнего результата без вызова провайдера
	res2, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("second CreatePayment failed: %v", err)
	}
	if !res2.IsReplay {
		t.Fatal("second call must be replay")
	}
	if res2.Payment.ID != res1.Payment.ID {
		t.Errorf("expected same payment id %d, got %d", res1.Payment.ID, res2.Payment.ID)
	}
	if prov.calls.Load() != 1 {
		t.Errorf("provider must not be called again on replay, got %d calls", prov.calls.Load())
	}

	// 3. Тот же ключ, но другой заказ — ошибка конфликта 409
	_, err = svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        11,
		IdempotencyKey: key,
	})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Errorf("expected ErrIdempotencyConflict, got %v", err)
	}
}

func TestPaymentService_ProviderFailureDoesNotBlockOrder(t *testing.T) {
	svc, store, prov, _ := setupTestService(t)
	ctx := context.Background()

	prov.failCheckout = true
	prov.checkoutErr = provider.ErrDefinitiveRejection

	// Попытка 1: Провайдер отклонил запрос (4xx)
	_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "failed-key-111111111",
	})
	if err == nil {
		t.Fatal("expected error when provider fails, got nil")
	}

	// Проверяем, что платёж не завис в pending, а помечен как failed
	p, err := store.Payments().GetByIdempotencyKey(ctx, 1, "failed-key-111111111")
	if err != nil {
		t.Fatalf("payment must be recorded: %v", err)
	}
	if p.Status != domain.PaymentStatusFailed {
		t.Errorf("expected payment status to be 'failed' after provider error, got %s", p.Status)
	}

	// Проверяем аудит в payment_events
	events, err := store.PaymentEvents().ListByPaymentID(ctx, p.ID)
	if err != nil || len(events) < 2 {
		t.Fatalf("expected audit events for failed payment, got: %v", events)
	}

	// Теперь провайдер восстановился: пробуем снова для того же заказа с новым ключом
	prov.failCheckout = false
	res, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "retry-key-222222222",
	})
	if err != nil {
		t.Fatalf("order must NOT be blocked after previous provider failure: %v", err)
	}
	if res.Payment.Status != domain.PaymentStatusPending {
		t.Errorf("expected new payment to be pending, got %s", res.Payment.Status)
	}
}

func TestPaymentService_ProviderIdempotencyKeyFormat(t *testing.T) {
	svc, _, prov, _ := setupTestService(t)
	ctx := context.Background()

	clientKey := "client-key-12345678"
	res, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: clientKey,
	})
	if err != nil {
		t.Fatalf("CreatePayment failed: %v", err)
	}

	expectedProviderKey := fmt.Sprintf("pay-%d", res.Payment.ID)
	prov.mu.Lock()
	gotKey := prov.lastIdempotencyKey
	prov.mu.Unlock()

	if gotKey != expectedProviderKey {
		t.Fatalf("expected provider Idempotency-Key to be %q, got %q", expectedProviderKey, gotKey)
	}
}

func TestPaymentService_ProviderAmbiguousVsDefinitive(t *testing.T) {
	svc, store, prov, _ := setupTestService(t)
	ctx := context.Background()

	// 1. Неоднозначный исход (таймаут / 5xx): платёж ОБЯЗАН остаться pending! Заказ не освобождается.
	prov.failCheckout = true
	prov.checkoutErr = provider.ErrAmbiguousOutcome
	_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "ambiguous-key-111111",
	})
	if err == nil {
		t.Fatal("expected error on provider timeout")
	}

	p1, err := store.Payments().GetByIdempotencyKey(ctx, 1, "ambiguous-key-111111")
	if err != nil {
		t.Fatalf("payment must exist: %v", err)
	}
	if p1.Status != domain.PaymentStatusPending {
		t.Fatalf("expected payment to remain pending on ambiguous outcome, got %s", p1.Status)
	}

	// Повторный запрос с НОВЫМ ключом для того же заказа должен получить ErrActivePaymentExists
	_, errActive := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "new-key-after-timeout",
	})
	if !errors.Is(errActive, domain.ErrActivePaymentExists) {
		t.Fatalf("expected ErrActivePaymentExists for order with pending payment, got %v", errActive)
	}

	// 2. Однозначный отказ (400 / client error): платёж переводится в failed, заказ освобождается
	store.SeedOrder(&domain.Order{ID: 30, UserID: 1, AmountMinor: 5000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
	prov.failCheckout = true
	prov.checkoutErr = provider.ErrDefinitiveRejection

	_, errDef := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        30,
		IdempotencyKey: "definitive-key-11111",
	})
	if errDef == nil {
		t.Fatal("expected error on provider 400 rejection")
	}

	p2, err := store.Payments().GetByIdempotencyKey(ctx, 1, "definitive-key-11111")
	if err != nil {
		t.Fatalf("payment must exist: %v", err)
	}
	if p2.Status != domain.PaymentStatusFailed {
		t.Fatalf("expected payment to be failed on definitive rejection, got %s", p2.Status)
	}

	// Повторный запрос с НОВЫМ ключом для того же заказа освобождённого от pending должен быть успешен
	prov.failCheckout = false
	resNew, errNew := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        30,
		IdempotencyKey: "new-key-after-400-failed",
	})
	if errNew != nil {
		t.Fatalf("order 30 must be payable after failed payment, got: %v", errNew)
	}
	if resNew.Payment.Status != domain.PaymentStatusPending {
		t.Fatalf("expected new payment pending, got %s", resNew.Payment.Status)
	}
}

func TestPaymentService_FastReadReplayOnPaidOrder(t *testing.T) {
	svc, store, _, _ := setupTestService(t)
	ctx := context.Background()

	key := "paid-order-replay-key-1"
	res, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("initial CreatePayment failed: %v", err)
	}

	// Имитируем успешную оплату: заказ переходит в 'paid', платёж в 'succeeded'
	if err := store.Payments().UpdateStatus(ctx, res.Payment.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
		t.Fatalf("update payment: %v", err)
	}
	if err := store.Orders().UpdateStatusTx(ctx, nil, 10, domain.OrderStatusUnpaid, domain.OrderStatusPaid); err != nil {
		t.Fatalf("update order: %v", err)
	}

	// Повторный запрос с тем же Idempotency-Key ДОЛЖЕН вернуть replay со статусом succeeded, несмотря на order.status == 'paid'
	replayRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("replay of succeeded payment on paid order must NOT fail, got: %v", err)
	}
	if !replayRes.IsReplay {
		t.Fatalf("expected IsReplay=true")
	}
	if replayRes.Payment.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected replay status succeeded, got %s", replayRes.Payment.Status)
	}
}

func TestPaymentService_FastReadFailedReturnsErrPaymentNotPayable(t *testing.T) {
	svc, store, _, _ := setupTestService(t)
	ctx := context.Background()

	key := "failed-payment-key-1"
	res, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("initial CreatePayment: %v", err)
	}

	// Переводим платёж в failed
	if err := store.Payments().UpdateStatus(ctx, res.Payment.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	// Повторный вызов с тем же Idempotency-Key ОБЯЗАН возвращать ErrPaymentNotPayable (409, "используйте новый Idempotency-Key"), НЕ 200
	_, errReplay := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if !errors.Is(errReplay, domain.ErrPaymentNotPayable) {
		t.Fatalf("expected domain.ErrPaymentNotPayable for failed payment replay, got: %v", errReplay)
	}
}

func TestPaymentService_ForeignOrderReturnsErrOrderNotFound(t *testing.T) {
	svc, store, _, _ := setupTestService(t)
	ctx := context.Background()

	store.SeedUser(&domain.User{
		ID:        3,
		Email:     "charlie@example.kz",
		IsActive:  true,
		IsBlocked: false,
	})

	// Заказ 10 принадлежит пользователю 1. Пользователь 3 пытается создать платёж.
	// Должен получить ErrOrderNotFound (защита от перебора IDOR), а не ErrOrderForbidden.
	_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         3,
		OrderID:        10,
		IdempotencyKey: "foreign-order-key-11",
	})
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("expected domain.ErrOrderNotFound for foreign order, got: %v", err)
	}
}

type mockPaymentRepo struct {
	service.PaymentRepository
	createPendingErr error
	updateSessionErr error
}

func (m *mockPaymentRepo) CreatePending(ctx context.Context, p *domain.Payment) error {
	if m.createPendingErr != nil {
		return m.createPendingErr
	}
	return m.PaymentRepository.CreatePending(ctx, p)
}

func (m *mockPaymentRepo) UpdateSession(ctx context.Context, paymentID int64, providerPaymentID, checkoutURL string) error {
	if m.updateSessionErr != nil {
		return m.updateSessionErr
	}
	return m.PaymentRepository.UpdateSession(ctx, paymentID, providerPaymentID, checkoutURL)
}

func TestPaymentService_DatabaseIsLocked_ReturnsInternalError(t *testing.T) {
	_, store, prov, secret := setupTestService(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mockRepo := &mockPaymentRepo{
		PaymentRepository: store.Payments(),
		createPendingErr:  errors.New("database is locked"),
	}

	svc := service.NewPaymentService(
		store.Users(),
		store.Orders(),
		mockRepo,
		store.Webhooks(),
		store.PaymentEvents(),
		store.SecurityEvents(),
		store,
		prov,
		logger,
		secret,
	)

	_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "locked-db-key-12345",
	})
	if err == nil {
		t.Fatal("expected error when database is locked, got nil")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("expected error message to contain 'database is locked', got %v", err)
	}
	if errors.Is(err, domain.ErrActivePaymentExists) {
		t.Errorf("must NOT return ErrActivePaymentExists on database lock, got %v", err)
	}
	if errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Errorf("must NOT return ErrIdempotencyConflict on database lock, got %v", err)
	}
}

func TestPaymentService_UpdateSessionFailure_PaymentRemainsPending(t *testing.T) {
	_, store, prov, secret := setupTestService(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mockRepo := &mockPaymentRepo{
		PaymentRepository: store.Payments(),
		updateSessionErr:  errors.New("db connection lost during session update"),
	}

	svc := service.NewPaymentService(
		store.Users(),
		store.Orders(),
		mockRepo,
		store.Webhooks(),
		store.PaymentEvents(),
		store.SecurityEvents(),
		store,
		prov,
		logger,
		secret,
	)

	key := "update-session-fail-key"
	_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: key,
	})
	if err == nil {
		t.Fatal("expected error when UpdateSession fails, got nil")
	}
	if !strings.Contains(err.Error(), "update payment session") {
		t.Errorf("expected error to wrap 'update payment session', got: %v", err)
	}

	// Проверяем, что провайдер был вызван
	if prov.calls.Load() != 1 {
		t.Errorf("expected provider to have been called once, got %d", prov.calls.Load())
	}

	// Проверяем п. 2.6: платёж остаётся в БД со статусом pending (для reconciler)
	p, getErr := store.Payments().GetByIdempotencyKey(ctx, 1, key)
	if getErr != nil {
		t.Fatalf("payment must still be recorded in storage: %v", getErr)
	}
	if p.Status != domain.PaymentStatusPending {
		t.Errorf("expected payment to remain pending for reconciliation, got %s", p.Status)
	}
}

// -------------------------------------------------------------
// Тесты конкуренции:
// а) 20 горутин, ОДИН ключ
// б) 20 горутин, РАЗНЫЕ ключи, один заказ
// в) оба сценария на sqlite и in-memory
// -------------------------------------------------------------

func runConcurrentSameKeyTest(t *testing.T, svc *service.PaymentService, prov *fakeProvider, getPaymentCount func() int) {
	const goroutines = 20
	key := fmt.Sprintf("race-same-key-%d", time.Now().UnixNano())

	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]service.CreatePaymentResult, goroutines)
	errorsList := make([]error, goroutines)

	startGate := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startGate
			res, err := svc.CreatePayment(context.Background(), service.CreatePaymentInput{
				UserID:         1,
				OrderID:        10,
				IdempotencyKey: key,
			})
			results[idx] = res
			errorsList[idx] = err
		}(i)
	}

	close(startGate)
	wg.Wait()

	var firstPaymentID int64
	var successCount int

	for i := 0; i < goroutines; i++ {
		err := errorsList[i]
		if err == nil {
			successCount++
			if firstPaymentID == 0 {
				firstPaymentID = results[i].Payment.ID
			} else if results[i].Payment.ID != firstPaymentID {
				t.Fatalf("goroutine %d returned different payment id %d != %d", i, results[i].Payment.ID, firstPaymentID)
			}
		} else if !errors.Is(err, domain.ErrRequestInProgress) {
			t.Fatalf("goroutine %d received unexpected error: %v", i, err)
		}
	}

	if successCount == 0 {
		t.Fatalf("expected at least 1 successful goroutine (the winner), got 0")
	}
	if prov.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", prov.calls.Load())
	}
	if count := getPaymentCount(); count != 1 {
		t.Fatalf("expected exactly 1 payment in storage, got %d", count)
	}
}

func runConcurrentDifferentKeysTest(t *testing.T, svc *service.PaymentService, prov *fakeProvider, getPaymentCount func() int) {
	const goroutines = 20
	nowNano := time.Now().UnixNano()

	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]service.CreatePaymentResult, goroutines)
	errorsList := make([]error, goroutines)

	startGate := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		key := fmt.Sprintf("diff-key-%d-%d", nowNano, i)
		go func(idx int, k string) {
			defer wg.Done()
			<-startGate
			res, err := svc.CreatePayment(context.Background(), service.CreatePaymentInput{
				UserID:         1,
				OrderID:        10,
				IdempotencyKey: k,
			})
			results[idx] = res
			errorsList[idx] = err
		}(i, key)
	}

	close(startGate)
	wg.Wait()

	var successCount, conflictCount int
	for i := 0; i < goroutines; i++ {
		err := errorsList[i]
		if err == nil {
			successCount++
		} else if errors.Is(err, domain.ErrActivePaymentExists) {
			conflictCount++
		} else {
			t.Fatalf("goroutine %d received unexpected error: %v", i, err)
		}
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 success, got %d", successCount)
	}
	if conflictCount != goroutines-1 {
		t.Fatalf("expected %d ErrActivePaymentExists, got %d", goroutines-1, conflictCount)
	}
	if prov.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", prov.calls.Load())
	}
	if count := getPaymentCount(); count != 1 {
		t.Fatalf("expected exactly 1 pending payment in storage, got %d", count)
	}
}

func TestPaymentService_ConcurrentRace20Goroutines_SameKey_Memory(t *testing.T) {
	svc, store, prov, _ := setupTestService(t)
	runConcurrentSameKeyTest(t, svc, prov, func() int {
		p, err := store.Payments().GetByID(context.Background(), 1)
		if err != nil || p == nil {
			return 0
		}
		return 1
	})
}

func TestPaymentService_ConcurrentRace20Goroutines_DifferentKeys_Memory(t *testing.T) {
	svc, store, prov, _ := setupTestService(t)
	runConcurrentDifferentKeysTest(t, svc, prov, func() int {
		p, err := store.Payments().GetByID(context.Background(), 1)
		if err != nil || p == nil {
			return 0
		}
		return 1
	})
}

func TestPaymentService_ConcurrentRace20Goroutines_SameKey_SQLite(t *testing.T) {
	svc, store, prov, _ := setupTestServiceSQLite(t)
	runConcurrentSameKeyTest(t, svc, prov, func() int {
		var count int
		if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM payments").Scan(&count); err != nil {
			t.Fatalf("scan count: %v", err)
		}
		return count
	})
}

func TestPaymentService_ConcurrentRace20Goroutines_DifferentKeys_SQLite(t *testing.T) {
	svc, store, prov, _ := setupTestServiceSQLite(t)
	runConcurrentDifferentKeysTest(t, svc, prov, func() int {
		var count int
		if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM payments").Scan(&count); err != nil {
			t.Fatalf("scan count: %v", err)
		}
		return count
	})
}

func TestPaymentService_Webhook_SignatureValidation(t *testing.T) {
	svc, _, _, secret := setupTestService(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "webhook-test-key-12345",
	})
	if err != nil {
		t.Fatalf("setup payment failed: %v", err)
	}

	payload := service.WebhookPayload{
		EventID:           "evt_001",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)

	t.Run("valid_signature", func(t *testing.T) {
		now := time.Now().Unix()
		sig := signWebhook(secret, now, rawBody)

		err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now))
		if err != nil {
			t.Fatalf("expected valid webhook to pass, got: %v", err)
		}
	})

	t.Run("invalid_signature", func(t *testing.T) {
		now := time.Now().Unix()
		fakeSig := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

		err := svc.ProcessWebhook(ctx, rawBody, fakeSig, fmt.Sprintf("%d", now))
		if !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("expected ErrInvalidSignature, got %v", err)
		}
	})
}

func TestPaymentService_Webhook_ProviderIDMismatch_CommitsAndPersistsAudit(t *testing.T) {
	svc, store, _, secret := setupTestServiceSQLite(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "mismatch-prov-id-key-1",
	})
	if err != nil {
		t.Fatalf("setup payment failed: %v", err)
	}

	payload := service.WebhookPayload{
		EventID:           "evt_spoofed_prov_id",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: "ch_attacker_999", // НЕ СОВПАДАЕТ
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	// 3.1: Ошибка возвращается после коммита
	err = svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now))
	if !errors.Is(err, domain.ErrProviderIDMismatch) {
		t.Fatalf("expected ErrProviderIDMismatch, got %v", err)
	}

	// 3.1: Транзакция КОММИТИТСЯ: проверяем обе таблицы (webhook_events и security_events)
	var webhookCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM webhook_events WHERE event_id = ?", "evt_spoofed_prov_id").Scan(&webhookCount); err != nil {
		t.Fatalf("scan webhook_events: %v", err)
	}
	if webhookCount != 1 {
		t.Fatalf("expected webhook_events to be committed and have 1 entry, got %d", webhookCount)
	}

	var secCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM security_events WHERE event_type = 'security.provider_id_mismatch'").Scan(&secCount); err != nil {
		t.Fatalf("scan security_events: %v", err)
	}
	if secCount != 1 {
		t.Fatalf("expected security_events to be committed and have 1 entry, got %d", secCount)
	}

	// Повторный вебхук с тем же event_id должен вернуть 200 (nil), так как event_id уже зафиксирован
	errDup := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now))
	if errDup != nil {
		t.Fatalf("replay of mismatched webhook must return nil due to committed webhook_event, got %v", errDup)
	}
}

func TestPaymentService_Webhook_AmountMismatch_CommitsAndPersistsAudit(t *testing.T) {
	svc, store, _, secret := setupTestServiceSQLite(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "mismatch-amt-key-1111",
	})
	if err != nil {
		t.Fatalf("setup payment failed: %v", err)
	}

	payload := service.WebhookPayload{
		EventID:           "evt_amount_mismatch_1",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       10000, // Меньше суммы платежа (50000)
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	// 3.1: Ошибка возвращается после коммита
	err = svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now))
	if !errors.Is(err, domain.ErrWebhookAmountMismatch) {
		t.Fatalf("expected ErrWebhookAmountMismatch, got %v", err)
	}

	// 3.1: Транзакция КОММИТИТСЯ: проверяем обе таблицы
	var webhookCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM webhook_events WHERE event_id = ?", "evt_amount_mismatch_1").Scan(&webhookCount); err != nil {
		t.Fatalf("scan webhook_events: %v", err)
	}
	if webhookCount != 1 {
		t.Fatalf("expected webhook_events to have 1 entry, got %d", webhookCount)
	}

	var secCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM security_events WHERE event_type = 'security.amount_mismatch'").Scan(&secCount); err != nil {
		t.Fatalf("scan security_events: %v", err)
	}
	if secCount != 1 {
		t.Fatalf("expected security_events to have 1 entry, got %d", secCount)
	}
}

func TestPaymentService_Webhook_AmountCheckedAgainstPaymentNotOrder(t *testing.T) {
	svc, store, _, secret := setupTestServiceSQLite(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "check-against-pay-key",
	})
	if err != nil {
		t.Fatalf("create payment: %v", err)
	}

	// 3.2: Сверка суммы и валюты с payment.AmountMinor/Currency, а не с заказом.
	// Меняем сумму в заказе на 99999 (например, цена заказа изменилась после создания платежа)
	_, err = store.DB().ExecContext(ctx, "UPDATE orders SET amount_minor = 99999 WHERE id = 10")
	if err != nil {
		t.Fatalf("update order: %v", err)
	}

	// Вебхук отправляет 50000 (точная сумма платежа, но НЕ сумма заказа)
	payload := service.WebhookPayload{
		EventID:           "evt_match_payment_amount",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	if err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("expected webhook matching payment amount to succeed, got: %v", err)
	}

	p, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
	if p.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected payment status succeeded, got %s", p.Status)
	}
}

func TestPaymentService_Webhook_OrderAlreadyPaid_DuplicateRequiresRefund(t *testing.T) {
	svc, store, _, secret := setupTestServiceSQLite(t)
	ctx := context.Background()

	// Платёж 1 для заказа 10
	pay1, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "pay-1-key-11111111",
	})
	if err != nil {
		t.Fatalf("create pay1: %v", err)
	}

	// Платёж 1 успешно подтверждается вебхуком -> заказ 10 становится paid
	payload1 := service.WebhookPayload{
		EventID:           "evt_pay1_success",
		EventType:         "payment.succeeded",
		PaymentID:         pay1.Payment.ID,
		ProviderPaymentID: pay1.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody1, _ := json.Marshal(payload1)
	now := time.Now().Unix()
	sig1 := signWebhook(secret, now, rawBody1)

	if err := svc.ProcessWebhook(ctx, rawBody1, sig1, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("pay1 webhook failed: %v", err)
	}

	// Создаём платёж 2 в БД для заказа 10 со статусом pending (имитация гонки двух платежей)
	pay2 := &domain.Payment{
		UserID:            1,
		OrderID:           10,
		AmountMinor:       50000,
		Currency:          "KZT",
		Status:            domain.PaymentStatusPending,
		IdempotencyKey:    "pay-2-key-22222222",
		RequestHash:       "hash2",
		ProviderPaymentID: "ch_prov_pay2",
		CheckoutURL:       "https://checkout.fake/pay2",
	}
	if err := store.Payments().CreatePending(ctx, pay2); err != nil {
		t.Fatalf("create pending pay2: %v", err)
	}

	// 3.3 Приходит вебхук payment.succeeded для pay2, когда заказ УЖЕ paid:
	// коммит, событие payment.duplicate_requires_refund, без бесконечных повторов
	payload2 := service.WebhookPayload{
		EventID:           "evt_pay2_success_duplicate",
		EventType:         "payment.succeeded",
		PaymentID:         pay2.ID,
		ProviderPaymentID: pay2.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody2, _ := json.Marshal(payload2)
	now = time.Now().Unix()
	sig2 := signWebhook(secret, now, rawBody2)

	if err := svc.ProcessWebhook(ctx, rawBody2, sig2, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("duplicate payment webhook must succeed (return nil for commit), got: %v", err)
	}

	// Проверяем запись события payment.duplicate_requires_refund
	events, err := store.PaymentEvents().GetManualReviewEvents(ctx)
	if err != nil {
		t.Fatalf("get manual review events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.PaymentID == pay2.ID && e.EventType == "payment.duplicate_requires_refund" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected payment.duplicate_requires_refund in manual review events, got: %v", events)
	}
}

func TestPaymentService_Webhook_Succeeded_RequiresProviderPaymentID(t *testing.T) {
	svc, _, _, secret := setupTestService(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "missing-prov-id-key",
	})
	if err != nil {
		t.Fatalf("create payment: %v", err)
	}

	// 3.4 payment.succeeded требует непустой ProviderPaymentID в вебхуке; если пуст в вебхуке — отказ
	payload := service.WebhookPayload{
		EventID:           "evt_missing_prov_id",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: "", // ПУСТО
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	err = svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now))
	if err == nil {
		t.Fatal("expected error on payment.succeeded with empty ProviderPaymentID, got nil")
	}
}

func TestPaymentService_Webhook_Succeeded_AttachesProviderPaymentIDWhenEmpty(t *testing.T) {
	svc, store, _, secret := setupTestService(t)
	ctx := context.Background()

	// Платёж без provider_payment_id (например, произошёл сбой UpdateSession после обращения к провайдеру)
	payment := &domain.Payment{
		UserID:         1,
		OrderID:        10,
		AmountMinor:    50000,
		Currency:       "KZT",
		Status:         domain.PaymentStatusPending,
		IdempotencyKey: "attach-prov-id-key-1",
		RequestHash:    "hash-attach-1",
	}
	if err := store.Payments().CreatePending(ctx, payment); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	// 3.4 если у платежа provider_payment_id пуст — привяжи и запиши событие
	payload := service.WebhookPayload{
		EventID:           "evt_attach_success_1",
		EventType:         "payment.succeeded",
		PaymentID:         payment.ID,
		ProviderPaymentID: "ch_prov_newly_attached_123",
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	if err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("process webhook: %v", err)
	}

	p, _ := store.Payments().GetByID(ctx, payment.ID)
	if p.ProviderPaymentID != "ch_prov_newly_attached_123" {
		t.Fatalf("expected provider_payment_id attached, got %q", p.ProviderPaymentID)
	}
	if p.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected status succeeded, got %s", p.Status)
	}

	events, _ := store.PaymentEvents().ListByPaymentID(ctx, payment.ID)
	foundAttach := false
	for _, e := range events {
		if e.EventType == "payment.provider_id_attached" {
			foundAttach = true
			break
		}
	}
	if !foundAttach {
		t.Errorf("expected payment.provider_id_attached event recorded")
	}
}

func TestPaymentService_Webhook_UnknownPaymentID_RecordsSecurityEventAndReturns200(t *testing.T) {
	svc, store, _, secret := setupTestServiceSQLite(t)
	ctx := context.Background()

	// 3.5 Неизвестный payment_id: security_events, ответ 200 (повторять нечего)
	payload := service.WebhookPayload{
		EventID:           "evt_unknown_pay_id",
		EventType:         "payment.succeeded",
		PaymentID:         99999999, // Не существует
		ProviderPaymentID: "ch_unknown_123",
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now))
	if err != nil {
		t.Fatalf("expected 200 OK (nil error) on unknown payment_id, got: %v", err)
	}

	var secCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM security_events WHERE event_type = 'security.unknown_payment_id'").Scan(&secCount); err != nil {
		t.Fatalf("query security_events: %v", err)
	}
	if secCount != 1 {
		t.Fatalf("expected 1 security.unknown_payment_id event recorded, got %d", secCount)
	}
}

func TestPaymentService_Webhook_LateSuccess_RequiresRefund(t *testing.T) {
	svc, store, _, secret := setupTestService(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "late-success-key-111",
	})
	if err != nil {
		t.Fatalf("create payment: %v", err)
	}

	// Переводим платёж в failed (финальный статус)
	if err := store.Payments().UpdateStatus(ctx, payRes.Payment.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	// 3.7 Late success (успех по failed/canceled): событие payment.late_success_requires_refund, строка в списке ручного разбора
	payload := service.WebhookPayload{
		EventID:           "evt_late_success_1",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	if err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("late success webhook must return nil (commit), got: %v", err)
	}

	// Проверяем, что финальный статус НЕ перезаписан
	p, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
	if p.Status != domain.PaymentStatusFailed {
		t.Fatalf("finalized status failed must NOT be overwritten, got %s", p.Status)
	}

	// Проверяем список ручного разбора
	events, err := store.PaymentEvents().GetManualReviewEvents(ctx)
	if err != nil {
		t.Fatalf("get manual review events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.PaymentID == payRes.Payment.ID && e.EventType == "payment.late_success_requires_refund" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected payment.late_success_requires_refund in manual review list, got: %v", events)
	}
}

func TestPaymentService_Webhook_DualSecretRotation(t *testing.T) {
	_, store, prov, _ := setupTestService(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	secretOld := "old-secret-key-11111111"
	secretNew := "new-secret-key-22222222"

	// 3.8 Два активных секрета вебхука одновременно (ротация без простоя)
	svc := service.NewPaymentService(
		store.Users(),
		store.Orders(),
		store.Payments(),
		store.Webhooks(),
		store.PaymentEvents(),
		store.SecurityEvents(),
		store,
		prov,
		logger,
		secretOld,
		secretNew,
	)

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "dual-secret-key-1111",
	})
	if err != nil {
		t.Fatalf("create payment: %v", err)
	}

	// 1. Вебхук, подписанный старым секретом — принимается
	payload1 := service.WebhookPayload{
		EventID:           "evt_signed_old_secret",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	raw1, _ := json.Marshal(payload1)
	now := time.Now().Unix()
	sigOld := signWebhook(secretOld, now, raw1)

	if err := svc.ProcessWebhook(ctx, raw1, sigOld, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("webhook signed with old secret must pass, got: %v", err)
	}

	// 2. Вебхук, подписанный новым секретом — принимается
	payload2 := service.WebhookPayload{
		EventID:           "evt_signed_new_secret",
		EventType:         "payment.failed",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	raw2, _ := json.Marshal(payload2)
	now = time.Now().Unix()
	sigNew := signWebhook(secretNew, now, raw2)

	if err := svc.ProcessWebhook(ctx, raw2, sigNew, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("webhook signed with new secret must pass, got: %v", err)
	}

	// 3. Вебхук, подписанный невалидным секретом — отклоняется
	sigBad := signWebhook("invalid-secret-key-9999", now, raw2)
	payload3 := service.WebhookPayload{
		EventID:   "evt_signed_bad_secret",
		PaymentID: payRes.Payment.ID,
	}
	raw3, _ := json.Marshal(payload3)
	errBad := svc.ProcessWebhook(ctx, raw3, sigBad, fmt.Sprintf("%d", now))
	if !errors.Is(errBad, domain.ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature on bad secret, got: %v", errBad)
	}
}

func TestPaymentService_Webhook_DuplicateDeduplication(t *testing.T) {
	svc, store, _, secret := setupTestService(t)
	ctx := context.Background()

	payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "dup-webhook-key-12345",
	})
	if err != nil {
		t.Fatalf("create payment failed: %v", err)
	}

	payload := service.WebhookPayload{
		EventID:           "evt_unique_123",
		EventType:         "payment.succeeded",
		PaymentID:         payRes.Payment.ID,
		ProviderPaymentID: payRes.Payment.ProviderPaymentID,
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	rawBody, _ := json.Marshal(payload)
	now := time.Now().Unix()
	sig := signWebhook(secret, now, rawBody)

	if err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("first webhook delivery failed: %v", err)
	}

	order, _ := store.Orders().GetByID(ctx, 10)
	if order.Status != domain.OrderStatusPaid {
		t.Fatalf("expected order status paid, got %s", order.Status)
	}

	if err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", now)); err != nil {
		t.Fatalf("duplicate webhook delivery must succeed without error, got: %v", err)
	}
}

func TestReconciler_StalePendingSweeper(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store.SeedOrder(&domain.Order{
		ID:          50,
		UserID:      1,
		AmountMinor: 10000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})
	store.SeedOrder(&domain.Order{
		ID:          51,
		UserID:      1,
		AmountMinor: 20000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	oldTime := time.Now().Add(-30 * time.Minute)
	p1 := &domain.Payment{
		UserID:         1,
		OrderID:        50,
		AmountMinor:    10000,
		Currency:       "KZT",
		IdempotencyKey: "crashed-key-1111111",
		RequestHash:    "hash1",
		CreatedAt:      oldTime,
	}
	if err := store.Payments().CreatePending(context.Background(), p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}

	p2 := &domain.Payment{
		UserID:         1,
		OrderID:        51,
		AmountMinor:    20000,
		Currency:       "KZT",
		IdempotencyKey: "success-key-2222222",
		RequestHash:    "hash2",
		CreatedAt:      oldTime,
	}
	if err := store.Payments().CreatePending(context.Background(), p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	if err := store.Payments().UpdateSession(context.Background(), p2.ID, "ch_prov_p2", "https://pay"); err != nil {
		t.Fatalf("update p2 session: %v", err)
	}

	// 4.1: Для p1 (пустой provider_payment_id) провайдер опрашивается по ключу pay-<p1.ID>
	prov := &fakeProvider{
		statusByID: map[string]domain.PaymentStatus{
			fmt.Sprintf("pay-%d", p1.ID): domain.PaymentStatusFailed,
			"ch_prov_p2":                 domain.PaymentStatusSucceeded,
		},
	}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{
			TTL:      15 * time.Minute,
			Interval: 10 * time.Millisecond,
			Batch:    10,
		},
	)

	count, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile run failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 payments to be reconciled, got %d", count)
	}

	p1Updated, _ := store.Payments().GetByID(context.Background(), p1.ID)
	if p1Updated.Status != domain.PaymentStatusFailed {
		t.Errorf("expected p1 to be failed, got %s", p1Updated.Status)
	}

	p2Updated, _ := store.Payments().GetByID(context.Background(), p2.ID)
	if p2Updated.Status != domain.PaymentStatusSucceeded {
		t.Errorf("expected p2 to be succeeded, got %s", p2Updated.Status)
	}
	o51, _ := store.Orders().GetByID(context.Background(), 51)
	if o51.Status != domain.OrderStatusPaid {
		t.Errorf("expected order 51 to be paid, got %s", o51.Status)
	}
}

func TestReconciler_EmptyProviderID_AlwaysQueriesProviderByKey(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store.SeedOrder(&domain.Order{
		ID:          60,
		UserID:      1,
		AmountMinor: 15000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	oldTime := time.Now().Add(-20 * time.Minute)
	p := &domain.Payment{
		UserID:         1,
		OrderID:        60,
		AmountMinor:    15000,
		Currency:       "KZT",
		IdempotencyKey: "empty-prov-id-key-1",
		RequestHash:    "hash_empty_prov",
		CreatedAt:      oldTime,
	}
	if err := store.Payments().CreatePending(context.Background(), p); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	expectedKey := fmt.Sprintf("pay-%d", p.ID)
	prov := &fakeProvider{
		statusByID: map[string]domain.PaymentStatus{
			expectedKey: domain.PaymentStatusFailed,
		},
	}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 10},
	)

	count, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 reconciled, got %d", count)
	}

	// Проверяем, что провайдер БЫЛ вызван по ключу pay-<id>
	prov.mu.Lock()
	queriedKey := prov.lastStatusCheckID
	prov.mu.Unlock()
	if queriedKey != expectedKey {
		t.Fatalf("expected provider to be queried with key %q, got %q", expectedKey, queriedKey)
	}

	updated, _ := store.Payments().GetByID(context.Background(), p.ID)
	if updated.Status != domain.PaymentStatusFailed {
		t.Fatalf("expected payment status failed, got %s", updated.Status)
	}
}

func TestReconciler_EmptyProviderID_ProviderSucceeded_MarksPaymentSucceeded(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store.SeedOrder(&domain.Order{
		ID:          70,
		UserID:      1,
		AmountMinor: 15000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	oldTime := time.Now().Add(-20 * time.Minute)
	p := &domain.Payment{
		UserID:         1,
		OrderID:        70,
		AmountMinor:    15000,
		Currency:       "KZT",
		IdempotencyKey: "empty-prov-id-recovered-key",
		RequestHash:    "hash_recovered",
		CreatedAt:      oldTime,
	}
	if err := store.Payments().CreatePending(context.Background(), p); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	expectedKey := fmt.Sprintf("pay-%d", p.ID)
	// Провайдер подтверждает, что сессия была оплачена!
	prov := &fakeProvider{
		statusByID: map[string]domain.PaymentStatus{
			expectedKey: domain.PaymentStatusSucceeded,
		},
	}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 10},
	)

	count, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 reconciled, got %d", count)
	}

	updated, _ := store.Payments().GetByID(context.Background(), p.ID)
	if updated.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected payment status succeeded, got %s", updated.Status)
	}
	if updated.ProviderPaymentID != expectedKey {
		t.Fatalf("expected ProviderPaymentID to be attached as %q, got %q", expectedKey, updated.ProviderPaymentID)
	}

	order, _ := store.Orders().GetByID(context.Background(), 70)
	if order.Status != domain.OrderStatusPaid {
		t.Fatalf("expected order status paid, got %s", order.Status)
	}
}

func TestReconciler_ProviderError_DoesNotMarkFailed(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store.SeedOrder(&domain.Order{
		ID:          80,
		UserID:      1,
		AmountMinor: 15000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	oldTime := time.Now().Add(-20 * time.Minute)
	p := &domain.Payment{
		UserID:            1,
		OrderID:           80,
		AmountMinor:       15000,
		Currency:          "KZT",
		Status:            domain.PaymentStatusPending,
		IdempotencyKey:    "prov-error-key-11",
		RequestHash:       "hash_err",
		ProviderPaymentID: "ch_prov_failing_status",
		CreatedAt:         oldTime,
	}
	if err := store.Payments().CreatePending(context.Background(), p); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	// Провайдер возвращает 5xx / сетевую ошибку при проверке статуса
	prov := &fakeProvider{
		statusCheckErr: errors.New("provider gateway 502 Bad Gateway"),
	}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 10},
	)

	count, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile run must not fail totally, got error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 reconciled due to provider error, got %d", count)
	}

	// 4.1: При ошибке провайдера платёж ОБЯЗАН остаться pending (не переводиться в failed!)
	updated, _ := store.Payments().GetByID(context.Background(), p.ID)
	if updated.Status != domain.PaymentStatusPending {
		t.Fatalf("payment must remain pending on provider check error, got: %s", updated.Status)
	}
}

func TestReconciler_SinglePaymentError_DoesNotStopBatch(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	oldTime := time.Now().Add(-25 * time.Minute)

	for i := int64(1); i <= 3; i++ {
		store.SeedOrder(&domain.Order{
			ID:          90 + i,
			UserID:      1,
			AmountMinor: 10000,
			Currency:    "KZT",
			Status:      domain.OrderStatusUnpaid,
		})
		p := &domain.Payment{
			UserID:            1,
			OrderID:           90 + i,
			AmountMinor:       10000,
			Currency:          "KZT",
			Status:            domain.PaymentStatusPending,
			IdempotencyKey:    fmt.Sprintf("batch-item-key-%d", i),
			RequestHash:       fmt.Sprintf("hash_batch_%d", i),
			ProviderPaymentID: fmt.Sprintf("ch_prov_batch_%d", i),
			CreatedAt:         oldTime,
		}
		if err := store.Payments().CreatePending(context.Background(), p); err != nil {
			t.Fatalf("create payment %d: %v", i, err)
		}
	}

	// Платеж 1 вернет ошибку, Платежи 2 и 3 — успешный ответ Failed от провайдера
	prov := &fakeProvider{
		errByID: map[string]error{
			"ch_prov_batch_1": errors.New("timeout checking status"),
		},
		statusByID: map[string]domain.PaymentStatus{
			"ch_prov_batch_2": domain.PaymentStatusFailed,
			"ch_prov_batch_3": domain.PaymentStatusCanceled,
		},
	}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 10},
	)

	// 4.2: Ошибка по одному платежу не останавливает цикл
	count, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile once failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 payments to be reconciled despite 1 failure, got %d", count)
	}

	p1, _ := store.Payments().GetByID(context.Background(), 1)
	if p1.Status != domain.PaymentStatusPending {
		t.Errorf("expected payment 1 to remain pending, got %s", p1.Status)
	}
	p2, _ := store.Payments().GetByID(context.Background(), 2)
	if p2.Status != domain.PaymentStatusFailed {
		t.Errorf("expected payment 2 to be failed, got %s", p2.Status)
	}
	p3, _ := store.Payments().GetByID(context.Background(), 3)
	if p3.Status != domain.PaymentStatusCanceled {
		t.Errorf("expected payment 3 to be canceled, got %s", p3.Status)
	}
}

func TestReconciler_BatchLimit(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	oldTime := time.Now().Add(-30 * time.Minute)

	for i := int64(1); i <= 5; i++ {
		store.SeedOrder(&domain.Order{
			ID:          200 + i,
			UserID:      1,
			AmountMinor: 10000,
			Currency:    "KZT",
			Status:      domain.OrderStatusUnpaid,
		})
		p := &domain.Payment{
			UserID:            1,
			OrderID:           200 + i,
			AmountMinor:       10000,
			Currency:          "KZT",
			Status:            domain.PaymentStatusPending,
			IdempotencyKey:    fmt.Sprintf("limit-key-%d", i),
			RequestHash:       fmt.Sprintf("hash_lim_%d", i),
			ProviderPaymentID: fmt.Sprintf("ch_prov_lim_%d", i),
			CreatedAt:         oldTime,
		}
		if err := store.Payments().CreatePending(context.Background(), p); err != nil {
			t.Fatalf("create payment %d: %v", i, err)
		}
	}

	prov := &fakeProvider{statusToReturn: domain.PaymentStatusFailed}

	// 4.2: Ограничение пакета (Batch = 2)
	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 2},
	)

	count, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile once failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected exactly 2 payments (Batch=2) to be reconciled, got %d", count)
	}
}

func TestReconciler_ContextCanceled_StopsGracefully(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prov := &fakeProvider{statusToReturn: domain.PaymentStatusFailed}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 10},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Отменяем контекст до запуска

	// 4.2: Корректная остановка по ctx
	_, err := reconciler.ReconcileOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}
}

func TestReconciler_DeterministicClock(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	baseTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	store.SeedOrder(&domain.Order{
		ID:          300,
		UserID:      1,
		AmountMinor: 10000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	// Платёж создан в 12:00
	p := &domain.Payment{
		UserID:            1,
		OrderID:           300,
		AmountMinor:       10000,
		Currency:          "KZT",
		Status:            domain.PaymentStatusPending,
		IdempotencyKey:    "clock-test-key-1",
		RequestHash:       "hash_clock",
		ProviderPaymentID: "ch_prov_clock",
		CreatedAt:         baseTime,
	}
	if err := store.Payments().CreatePending(context.Background(), p); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	prov := &fakeProvider{statusToReturn: domain.PaymentStatusFailed}

	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		service.ReconcilerConfig{TTL: 15 * time.Minute, Batch: 10},
	)

	// 4.2: Clock в тестах. В 12:10 (прошло 10 минут, TTL 15 минут) — платёж НЕ должен быть затронут
	reconciler.SetClock(domain.FrozenClock{Current: baseTime.Add(10 * time.Minute)})
	count1, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if count1 != 0 {
		t.Fatalf("expected 0 reconciled at +10m, got %d", count1)
	}

	// В 12:20 (прошло 20 минут, TTL 15 минут истёк) — платёж ДОЛЖЕН быть обработан
	reconciler.SetClock(domain.FrozenClock{Current: baseTime.Add(20 * time.Minute)})
	count2, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if count2 != 1 {
		t.Fatalf("expected 1 reconciled at +20m, got %d", count2)
	}

	updated, err := store.Payments().GetByID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if updated.Status != domain.PaymentStatusFailed {
		t.Fatalf("expected status failed after TTL expiry, got %s", updated.Status)
	}
}

func TestPaymentService_CreatePayment_BlockedUser_ReturnsError(t *testing.T) {
	svc, store, _, _ := setupTestService(t)
	ctx := context.Background()

	store.SeedOrder(&domain.Order{
		ID:          99,
		UserID:      2, // User 2 is blocked
		AmountMinor: 1000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         2,
		OrderID:        99,
		IdempotencyKey: "blocked-user-key-12345",
	})
	if !errors.Is(err, domain.ErrUserBlocked) {
		t.Fatalf("expected ErrUserBlocked, got: %v", err)
	}
}
