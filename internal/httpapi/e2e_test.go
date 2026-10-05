package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/httpapi"
	"github.com/Senjumarru/miniwallet/internal/metrics"
	"github.com/Senjumarru/miniwallet/internal/provider"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/sqlite"
)

// TestE2E_FullPaymentLifecycle_SQLite выполняет полный сквозной тест жизненного цикла платежа:
// Авторизация по JWT -> Создание платежа (201) -> Идемпотентный повтор (200) -> Вебхук провайдера (200) ->
// Проверка обновления состояний заказа и платежа в SQLite -> Повторный вебхук (200, дедупликация) -> Метрики Prometheus.
func TestE2E_FullPaymentLifecycle_SQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "e2e.db")
	migrationsDir := filepath.Join("..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite with migrations: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// 1. Подготовка пользователя и заказа
	store.SeedUser(&domain.User{ID: 10, Email: "e2e_user@example.kz", IsActive: true})
	store.SeedOrder(&domain.Order{
		ID:          100,
		UserID:      10,
		AmountMinor: 50000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	prov := &dummyProvider{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	whSecret := "e2e-webhook-secret-key-32bytes!!"
	jwtSecret := "e2e-jwt-secret-key-at-least-32-b!"

	reg := prometheus.NewRegistry()
	promMetrics := metrics.NewMetrics(reg)

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
		whSecret,
	)
	svc.SetMetrics(promMetrics)

	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithReadyChecker(store.DB().PingContext),
		httpapi.WithMetricsHandler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{})),
	)

	// 2. Выпуск JWT токена (инвариант 2: HS256, обязательный exp)
	jwtMgr := httpapi.NewJWTManager([]byte(jwtSecret))
	token, err := jwtMgr.GenerateToken(10, 15*time.Minute)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// 3. POST /payments — Первичное создание платежа
	idemKey := "e2e-idempotency-key-12345"
	createBody := []byte(`{"order_id":100}`)
	req1 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(createBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("Idempotency-Key", idemKey)

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on payment creation, got %d: %s", w1.Code, w1.Body.String())
	}

	var res1 struct {
		PaymentID   int64  `json:"payment_id"`
		OrderID     int64  `json:"order_id"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Status      string `json:"status"`
		CheckoutURL string `json:"checkout_url"`
		IsReplay    bool   `json:"is_replay"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &res1); err != nil {
		t.Fatalf("failed to unmarshal create payment response: %v", err)
	}

	if res1.PaymentID <= 0 || res1.OrderID != 100 || res1.AmountMinor != 50000 || res1.Currency != "KZT" {
		t.Fatalf("unexpected payment attributes: %+v", res1)
	}
	if res1.Status != string(domain.PaymentStatusPending) {
		t.Fatalf("expected payment pending, got %s", res1.Status)
	}
	if res1.CheckoutURL == "" || res1.IsReplay {
		t.Fatalf("expected valid checkout_url and is_replay=false, got URL=%q replay=%v", res1.CheckoutURL, res1.IsReplay)
	}

	// 4. Повторный POST /payments с тем же Idempotency-Key -> 200 OK, is_replay=true
	req2 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(createBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Idempotency-Key", idemKey)

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on idempotent replay, got %d: %s", w2.Code, w2.Body.String())
	}
	var res2 struct {
		PaymentID int64 `json:"payment_id"`
		IsReplay  bool  `json:"is_replay"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &res2); err != nil {
		t.Fatalf("failed to unmarshal replay response: %v", err)
	}
	if res2.PaymentID != res1.PaymentID || !res2.IsReplay {
		t.Fatalf("expected same payment id %d with is_replay=true, got id=%d replay=%v", res1.PaymentID, res2.PaymentID, res2.IsReplay)
	}

	// 5. POST /webhooks/provider — Доставка вебхука payment.succeeded от провайдера с HMAC подписью
	webhookPayload := []byte(fmt.Sprintf(`{
		"event_id": "e2e_evt_succ_001",
		"event_type": "payment.succeeded",
		"payment_id": %d,
		"provider_payment_id": "ch_test_123",
		"amount_minor": 50000,
		"currency": "KZT"
	}`, res1.PaymentID))

	now := time.Now().Unix()
	sig := sign(whSecret, now, webhookPayload)

	reqWh := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
	reqWh.Header.Set("Content-Type", "application/json")
	reqWh.Header.Set("X-Signature", sig)
	reqWh.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))

	wWh := httptest.NewRecorder()
	handler.ServeHTTP(wWh, reqWh)

	if wWh.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from webhook endpoint, got %d: %s", wWh.Code, wWh.Body.String())
	}

	// 6. Проверка состояния в базе данных SQLite (инварианты 1, 3, 6)
	pDB, err := store.Payments().GetByID(ctx, res1.PaymentID)
	if err != nil {
		t.Fatalf("failed to get payment from sqlite: %v", err)
	}
	if pDB.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected payment status succeeded in DB, got: %s", pDB.Status)
	}

	oDB, err := store.Orders().GetByID(ctx, 100)
	if err != nil {
		t.Fatalf("failed to get order from sqlite: %v", err)
	}
	if oDB.Status != domain.OrderStatusPaid {
		t.Fatalf("expected order status paid in DB, got: %s", oDB.Status)
	}

	events, err := store.PaymentEvents().ListByPaymentID(ctx, res1.PaymentID)
	if err != nil {
		t.Fatalf("failed to list payment events: %v", err)
	}
	hasSuccessEvent := false
	for _, e := range events {
		if e.EventType == "payment.succeeded" && e.ToStatus == "succeeded" {
			hasSuccessEvent = true
			break
		}
	}
	if !hasSuccessEvent {
		t.Fatalf("expected payment.succeeded event in payment_events, got: %+v", events)
	}

	// 7. Повторная доставка вебхука с тем же event_id -> 200 OK (дедупликация)
	reqWhDup := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
	reqWhDup.Header.Set("Content-Type", "application/json")
	reqWhDup.Header.Set("X-Signature", sig)
	reqWhDup.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))

	wWhDup := httptest.NewRecorder()
	handler.ServeHTTP(wWhDup, reqWhDup)

	if wWhDup.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on duplicate webhook, got %d: %s", wWhDup.Code, wWhDup.Body.String())
	}

	// 8. Проверка GET /readyz и GET /metrics
	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	wReady := httptest.NewRecorder()
	handler.ServeHTTP(wReady, reqReady)
	if wReady.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /readyz, got %d", wReady.Code)
	}

	reqMetrics := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	wMetrics := httptest.NewRecorder()
	handler.ServeHTTP(wMetrics, reqMetrics)
	if wMetrics.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /metrics, got %d", wMetrics.Code)
	}
	metricsBody := wMetrics.Body.String()
	if !strings.Contains(metricsBody, "miniwallet_payments_total") {
		t.Fatalf("expected metrics to contain miniwallet_payments_total, got: %s", metricsBody)
	}
}

type e2eReconcileProvider struct {
	statuses map[string]domain.PaymentStatus
}

func (p *e2eReconcileProvider) CreateCheckoutSession(ctx context.Context, in provider.CreateCheckoutInput) (provider.CheckoutSession, error) {
	providerPaymentID := fmt.Sprintf("ch_order_%d", in.OrderID)
	return provider.CheckoutSession{
		ProviderPaymentID: providerPaymentID,
		CheckoutURL:       "https://checkout.example.com/" + providerPaymentID,
	}, nil
}

func (p *e2eReconcileProvider) GetPaymentStatus(ctx context.Context, providerPaymentID string) (domain.PaymentStatus, error) {
	if st, ok := p.statuses[providerPaymentID]; ok {
		return st, nil
	}
	return domain.PaymentStatusPending, nil
}

// TestE2E_ReconciliationAndExpiry_SQLite проверяет сквозную интеграцию фонового воркера сверки:
// 1. Создание платежей через HTTP API для разных заказов.
// 2. Истечение TTL с заморозкой времени (FrozenClock).
// 3. Успешная сверка провайдера (pending -> succeeded, заказ -> paid).
// 4. Истечение времени ожидания провайдера (pending -> failed, заказ остаётся unpaid, доступен повтор).
// 5. Обработка дубликата платежа для уже оплаченного заказа без ошибок (audit event duplicate_requires_refund).
// 6. Запуск и корректная остановка фонового воркера Reconciler.Start по отмене контекста.
func TestE2E_ReconciliationAndExpiry_SQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "reconcile_e2e.db")
	migrationsDir := filepath.Join("..", "..", "migrations")

	store, err := sqlite.Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("failed to open sqlite with migrations: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// 1. Подготовка пользователя и 3 заказов
	store.SeedUser(&domain.User{ID: 20, Email: "reconcile_user@example.kz", IsActive: true})
	store.SeedOrder(&domain.Order{ID: 201, UserID: 20, AmountMinor: 10000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
	store.SeedOrder(&domain.Order{ID: 202, UserID: 20, AmountMinor: 20000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
	store.SeedOrder(&domain.Order{ID: 203, UserID: 20, AmountMinor: 30000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

	prov := &e2eReconcileProvider{
		statuses: map[string]domain.PaymentStatus{
			"ch_order_201": domain.PaymentStatusSucceeded,
			"ch_order_202": domain.PaymentStatusPending, // провайдер всё ещё не получил оплату -> истечёт по TTL
			"ch_order_203": domain.PaymentStatusSucceeded,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	whSecret := "e2e-webhook-secret-key-32bytes!!"
	jwtSecret := "e2e-jwt-secret-key-at-least-32-b!"

	reg := prometheus.NewRegistry()
	promMetrics := metrics.NewMetrics(reg)

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
		whSecret,
	)
	svc.SetMetrics(promMetrics)

	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithReadyChecker(store.DB().PingContext),
		httpapi.WithMetricsHandler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{})),
	)

	jwtMgr := httpapi.NewJWTManager([]byte(jwtSecret))
	token, err := jwtMgr.GenerateToken(20, 15*time.Minute)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	makePaymentRequest := func(orderID int64, idemKey string) int64 {
		body := []byte(fmt.Sprintf(`{"order_id":%d}`, orderID))
		req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", idemKey)

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created for order %d, got %d: %s", orderID, w.Code, w.Body.String())
		}

		var res struct {
			PaymentID int64  `json:"payment_id"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}
		if res.Status != string(domain.PaymentStatusPending) {
			t.Fatalf("expected pending status, got %s", res.Status)
		}
		return res.PaymentID
	}

	// 2. Создание платежей через HTTP API для всех 3 заказов
	pID1 := makePaymentRequest(201, "idem-reconcile-order-201")
	pID2 := makePaymentRequest(202, "idem-reconcile-order-202")
	pID3 := makePaymentRequest(203, "idem-reconcile-order-203")

	// Переводим заказ 203 в paid напрямую (имитация альтернативной оплаты / гонки)
	if err := store.Orders().UpdateStatus(ctx, 203, domain.OrderStatusUnpaid, domain.OrderStatusPaid); err != nil {
		t.Fatalf("failed to set order 203 to paid: %v", err)
	}

	// 3. Создание и настройка Reconciler
	reconcilerCfg := service.ReconcilerConfig{
		TTL:      15 * time.Minute,
		Interval: 20 * time.Millisecond,
		Batch:    10,
	}
	reconciler := service.NewReconciler(
		store.Payments(),
		store.Orders(),
		store.PaymentEvents(),
		store,
		prov,
		logger,
		reconcilerCfg,
	)
	reconciler.SetMetrics(promMetrics)

	// Сдвигаем время сверщика вперед на 30 минут, чтобы 15-минутный TTL истек
	futureTime := time.Now().UTC().Add(30 * time.Minute)
	reconciler.SetClock(domain.FrozenClock{Current: futureTime})

	// 4. Выполняем итерацию сверки ReconcileOnce
	reconciledCount, err := reconciler.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce returned error: %v", err)
	}
	if reconciledCount != 3 {
		t.Fatalf("expected 3 payments reconciled, got %d", reconciledCount)
	}

	// 5. Проверка заказа 201: стал succeeded, заказ paid
	p1, err := store.Payments().GetByID(ctx, pID1)
	if err != nil || p1.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected payment 1 succeeded, got status %v, err: %v", p1.Status, err)
	}
	o1, err := store.Orders().GetByID(ctx, 201)
	if err != nil || o1.Status != domain.OrderStatusPaid {
		t.Fatalf("expected order 201 paid, got status %v, err: %v", o1.Status, err)
	}
	evs1, err := store.PaymentEvents().ListByPaymentID(ctx, pID1)
	if err != nil {
		t.Fatalf("failed to list events for p1: %v", err)
	}
	hasSucceededEv := false
	for _, e := range evs1 {
		if e.EventType == "reconciliation.provider_succeeded" {
			hasSucceededEv = true
			break
		}
	}
	if !hasSucceededEv {
		t.Fatalf("expected reconciliation.provider_succeeded event for p1, got %+v", evs1)
	}

	// 6. Проверка заказа 202: платеж failed (expired), заказ остался unpaid
	p2, err := store.Payments().GetByID(ctx, pID2)
	if err != nil || p2.Status != domain.PaymentStatusFailed {
		t.Fatalf("expected payment 2 failed, got status %v, err: %v", p2.Status, err)
	}
	o2, err := store.Orders().GetByID(ctx, 202)
	if err != nil || o2.Status != domain.OrderStatusUnpaid {
		t.Fatalf("expected order 202 unpaid, got status %v, err: %v", o2.Status, err)
	}
	evs2, err := store.PaymentEvents().ListByPaymentID(ctx, pID2)
	if err != nil {
		t.Fatalf("failed to list events for p2: %v", err)
	}
	hasExpiredEv := false
	for _, e := range evs2 {
		if e.EventType == "reconciliation.expired" {
			hasExpiredEv = true
			break
		}
	}
	if !hasExpiredEv {
		t.Fatalf("expected reconciliation.expired event for p2, got %+v", evs2)
	}

	// Проверка повторной попытки оплаты заказа 202 после истечения старого платежа
	newPID := makePaymentRequest(202, "idem-reconcile-order-202-retry")
	if newPID <= 0 || newPID == pID2 {
		t.Fatalf("expected new payment created for order 202, got id %d", newPID)
	}

	// 7. Проверка заказа 203: уже оплачен, платеж 3 стал succeeded, записан duplicate_requires_refund
	p3, err := store.Payments().GetByID(ctx, pID3)
	if err != nil || p3.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected payment 3 succeeded, got status %v, err: %v", p3.Status, err)
	}
	evs3, err := store.PaymentEvents().ListByPaymentID(ctx, pID3)
	if err != nil {
		t.Fatalf("failed to list events for p3: %v", err)
	}
	hasRefundEv := false
	for _, e := range evs3 {
		if e.EventType == "reconciliation.duplicate_requires_refund" {
			hasRefundEv = true
			break
		}
	}
	if !hasRefundEv {
		t.Fatalf("expected reconciliation.duplicate_requires_refund event for p3, got %+v", evs3)
	}

	// 8. Проверка Reconciler.Start фонового цикла и корректной остановки по контексту
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		reconciler.Start(workerCtx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancelWorker()

	select {
	case <-workerDone:
		// worker stopped cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler worker did not stop after context cancellation")
	}
}
