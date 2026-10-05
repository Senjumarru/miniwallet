package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/httpapi"
	"github.com/Senjumarru/miniwallet/internal/provider"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

type dummyProvider struct{}

func (d *dummyProvider) CreateCheckoutSession(ctx context.Context, in provider.CreateCheckoutInput) (provider.CheckoutSession, error) {
	return provider.CheckoutSession{
		ProviderPaymentID: "ch_test_123",
		CheckoutURL:       "https://checkout.fake/pay/123",
	}, nil
}

func (d *dummyProvider) GetPaymentStatus(ctx context.Context, providerPaymentID string) (domain.PaymentStatus, error) {
	return domain.PaymentStatusPending, nil
}

type emptyURLProvider struct{}

func (e *emptyURLProvider) CreateCheckoutSession(ctx context.Context, in provider.CreateCheckoutInput) (provider.CheckoutSession, error) {
	return provider.CheckoutSession{
		ProviderPaymentID: "ch_empty_123",
		CheckoutURL:       "", // Пустой checkout_url для тестирования п. 5.2
	}, nil
}

func (e *emptyURLProvider) GetPaymentStatus(ctx context.Context, providerPaymentID string) (domain.PaymentStatus, error) {
	return domain.PaymentStatusPending, nil
}

func setupTestServer(t *testing.T, opts ...func(*httpapi.HandlerOptions)) (http.Handler, *memory.Storage, string, string, *service.PaymentService) {
	t.Helper()
	store := memory.New()
	prov := &dummyProvider{}
	secret := "test-secret-key-12345"
	jwtSecret := "test-jwt-secret-key-54321-secure"
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
		Email:     "bob@example.kz",
		IsActive:  true,
		IsBlocked: false,
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
		AmountMinor: 20000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	handler := httpapi.NewHandler(svc, logger, jwtSecret, opts...)
	return handler, store, secret, jwtSecret, svc
}

func issueToken(t *testing.T, secret string, userID int64) string {
	t.Helper()
	jwtMgr := httpapi.NewJWTManager([]byte(secret))
	token, err := jwtMgr.GenerateToken(userID, 1*time.Hour)
	if err != nil {
		t.Fatalf("failed to generate jwt token: %v", err)
	}
	return token
}

func sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", timestamp)))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHTTP_CreatePayment(t *testing.T) {
	handler, _, _, jwtSecret, _ := setupTestServer(t)
	token := issueToken(t, jwtSecret, 1)

	key := "idem-http-key-123456"

	reqBody := []byte(`{"order_id":10}`)
	req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Authorization", "Bearer "+token) // 5.1: Проверенный JWT

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	// Повторный запрос — 200 OK replay
	req2 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", key)
	req2.Header.Set("Authorization", "Bearer "+token)

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on replay, got %d", w2.Code)
	}

	// Конфликт (другой order_id с тем же ключом) — 409 Conflict с заголовком Retry-After (п. 5.2)
	req3 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":11}`)))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Idempotency-Key", key)
	req3.Header.Set("Authorization", "Bearer "+token)

	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %d", w3.Code)
	}
	if w3.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After: 1 on 409 Conflict, got: %s", w3.Header().Get("Retry-After"))
	}
}

func TestHTTP_CreatePayment_BypassAuthRejected(t *testing.T) {
	handler, _, _, _, _ := setupTestServer(t)

	// Попытка создать платёж без JWT токена
	reqBody := []byte(`{"user_id":1,"order_id":10}`)
	req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "unauth-key-12345678")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
}

// 5.1: Заголовок X-User-ID игнорируется (тест с подделкой)
func TestHTTP_CreatePayment_SpoofedXUserIDIgnored(t *testing.T) {
	handler, store, _, jwtSecret, _ := setupTestServer(t)
	// Токен выписан на UserID = 1
	token := issueToken(t, jwtSecret, 1)

	// Злоумышленник пытается передать X-User-ID: 999
	reqBody := []byte(`{"order_id":10}`)
	req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "spoofed-header-key-1")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-ID", "999") // Поддельный заголовок

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for authenticated User 1, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		PaymentID int64 `json:"payment_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	p, err := store.Payments().GetByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if p.UserID != 1 {
		t.Fatalf("expected payment UserID to be 1 from JWT, got %d (X-User-ID was not ignored!)", p.UserID)
	}
}

// 5.1: user_id в теле запроса игнорируется (тест с подделкой)
func TestHTTP_CreatePayment_SpoofedBodyUserIDIgnored(t *testing.T) {
	handler, store, _, jwtSecret, _ := setupTestServer(t)
	token := issueToken(t, jwtSecret, 1)

	// Злоумышленник передает в теле {"user_id": 999, "order_id": 10}
	reqBody := []byte(`{"user_id":999,"order_id":10}`)
	req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "spoofed-body-key-1")
	req.Header.Set("Authorization", "Bearer "+token)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for authenticated User 1, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		PaymentID int64 `json:"payment_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	p, err := store.Payments().GetByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if p.UserID != 1 {
		t.Fatalf("expected payment UserID to be 1 from JWT, got %d (body user_id was not ignored!)", p.UserID)
	}
}

// 5.1: Передача X-User-ID без JWT токена не даёт доступ
func TestHTTP_CreatePayment_XUserIDWithoutJWTReturns401(t *testing.T) {
	handler, _, _, _, _ := setupTestServer(t)

	reqBody := []byte(`{"order_id":10}`)
	req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "x-user-only-key-1")
	req.Header.Set("X-User-ID", "1") // Без заголовка Authorization

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized when Authorization header is missing, got %d", w.Code)
	}
}

// 5.1: Невалидный или просроченный JWT возвращает 401
func TestHTTP_CreatePayment_InvalidOrExpiredJWT(t *testing.T) {
	handler, _, _, _, _ := setupTestServer(t)

	t.Run("malformed_jwt", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "malformed-jwt-key")
		req.Header.Set("Authorization", "Bearer not-a-valid-jwt-token")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for malformed JWT, got %d", w.Code)
		}
	})

	t.Run("wrong_secret_jwt", func(t *testing.T) {
		wrongToken := issueToken(t, "completely-different-secret-key-12345", 1)
		req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "wrong-secret-key")
		req.Header.Set("Authorization", "Bearer "+wrongToken)

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for JWT signed with wrong secret, got %d", w.Code)
		}
	})
}

// 5.2: Никогда не 200 с пустым checkout_url (тест хендлера); Retry-After для 409
func TestHTTP_CreatePayment_Never200WithEmptyCheckoutURL(t *testing.T) {
	store := memory.New()
	emptyProv := &emptyURLProvider{}
	secret := "test-secret-123"
	jwtSecret := "test-jwt-secret-empty-url"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := service.NewPaymentService(
		store.Users(),
		store.Orders(),
		store.Payments(),
		store.Webhooks(),
		store.PaymentEvents(),
		store.SecurityEvents(),
		store,
		emptyProv,
		logger,
		secret,
	)

	store.SeedUser(&domain.User{
		ID:        1,
		Email:     "user1@test.kz",
		IsActive:  true,
		IsBlocked: false,
	})
	store.SeedOrder(&domain.Order{
		ID:          100,
		UserID:      1,
		AmountMinor: 15000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
	})

	handler := httpapi.NewHandler(svc, logger, jwtSecret)
	token := issueToken(t, jwtSecret, 1)

	reqBody := []byte(`{"order_id":100}`)
	req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "empty-checkout-url-key-1")
	req.Header.Set("Authorization", "Bearer "+token)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Должен быть 409 Conflict с Retry-After, НИ В КОЕМ СЛУЧАЕ НЕ 200 OK
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when checkout_url is empty, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After header for 409 empty checkout_url, got %s", w.Header().Get("Retry-After"))
	}
}

// 5.2: Retry-After для 409 при различных конфликтах
func TestHTTP_CreatePayment_RetryAfterOn409(t *testing.T) {
	handler, store, _, jwtSecret, _ := setupTestServer(t)
	token := issueToken(t, jwtSecret, 1)

	// Создаем первый pending платеж
	req1 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "active-race-key-1")
	req1.Header.Set("Authorization", "Bearer "+token)
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("create payment 1 failed: %d", w1.Code)
	}

	// Попытка создать второй платеж на тот же заказ с ДРУГИМ ключом -> ErrActivePaymentExists (409)
	req2 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "active-race-key-2")
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 for active payment exists, got %d", w2.Code)
	}
	if w2.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After: 1 header, got %s", w2.Header().Get("Retry-After"))
	}

	// Переводим платеж в failed и проверяем ErrPaymentNotPayable (409)
	var created struct {
		PaymentID int64 `json:"payment_id"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal payment: %v", err)
	}
	if err := store.Payments().UpdateStatus(context.Background(), created.PaymentID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
		t.Fatalf("update status: %v", err)
	}

	req3 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Idempotency-Key", "active-race-key-1")
	req3.Header.Set("Authorization", "Bearer "+token)
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 for payment not payable, got %d", w3.Code)
	}
	if w3.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After: 1 header on ErrPaymentNotPayable, got %s", w3.Header().Get("Retry-After"))
	}
}

// 5.3: Проверка таймаутов http.Server
func TestHTTP_Server_TimeoutConfigurations(t *testing.T) {
	mux := http.NewServeMux()
	srv := httpapi.NewHTTPServer(":8080", mux)

	if srv.ReadHeaderTimeout != 2*time.Second {
		t.Errorf("expected ReadHeaderTimeout=2s, got %v", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 5*time.Second {
		t.Errorf("expected ReadTimeout=5s, got %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 10*time.Second {
		t.Errorf("expected WriteTimeout=10s, got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("expected IdleTimeout=60s, got %v", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("expected MaxHeaderBytes=1MB, got %v", srv.MaxHeaderBytes)
	}
}

// 5.4: Rate limit на создание платежей (по IP)
func TestHTTP_RateLimit_PaymentsIP(t *testing.T) {
	ipLimiter := httpapi.NewRateLimiter(1, 2, domain.RealClock{}) // Burst 2
	handler, _, _, jwtSecret, _ := setupTestServer(t, httpapi.WithPaymentsIPLimiter(ipLimiter))
	token := issueToken(t, jwtSecret, 1)

	// Запросы 1 и 2 должны пройти
	// Запросы 1 и 2 должны пройти (заказы 10 и 11 для User 1)
	orders := []int64{10, 11}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(fmt.Sprintf(`{"order_id":%d}`, orders[i]))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("valid-rl-ip-key-%04d", i+1))
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "192.168.1.100:12345"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("expected request %d to pass, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}

	// 3-й запрос превышает burst и должен получить 429 Too Many Requests
	req3 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Idempotency-Key", "valid-rl-ip-key-0003")
	req3.Header.Set("Authorization", "Bearer "+token)
	req3.RemoteAddr = "192.168.1.100:12345"

	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on IP limit breach, got %d: %s", w3.Code, w3.Body.String())
	}
	if w3.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

// 5.4: Rate limit на создание платежей (по пользователю)
func TestHTTP_RateLimit_PaymentsUser(t *testing.T) {
	userLimiter := httpapi.NewRateLimiter(1, 2, domain.RealClock{}) // Burst 2
	handler, _, _, jwtSecret, _ := setupTestServer(t, httpapi.WithPaymentsUserLimiter(userLimiter))
	token := issueToken(t, jwtSecret, 1)

	// Запросы с разных IP, но одного UserID=1 (заказы 10 и 11)
	orders := []int64{10, 11}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(fmt.Sprintf(`{"order_id":%d}`, orders[i]))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("valid-rl-usr-key-%04d", i+1))
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = fmt.Sprintf("10.0.0.%d:12345", i+1)

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("expected request %d to pass, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}

	// 3-й запрос пользователя превышает лимит
	req3 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Idempotency-Key", "valid-rl-usr-key-0003")
	req3.Header.Set("Authorization", "Bearer "+token)
	req3.RemoteAddr = "10.0.0.99:12345"

	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on User limit breach, got %d: %s", w3.Code, w3.Body.String())
	}
	if w3.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

// 5.4: Отдельный rate limit на вебхук
func TestHTTP_RateLimit_WebhookIP(t *testing.T) {
	whLimiter := httpapi.NewRateLimiter(1, 2, domain.RealClock{}) // Burst 2
	handler, _, secret, _, _ := setupTestServer(t, httpapi.WithWebhookLimiter(whLimiter))

	payload := []byte(`{"event_id":"evt_1","event_type":"payment.succeeded","payment_id":99999,"provider_payment_id":"ch_1"}`)
	now := time.Now().Unix()
	sig := sign(secret, now, payload)

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))
		req.RemoteAddr = "192.0.2.1:5000"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected webhook %d to pass, got %d", i, w.Code)
		}
	}

	// 3-й вебхук от того же IP должен быть отклонён 429
	req3 := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(payload))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Signature", sig)
	req3.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))
	req3.RemoteAddr = "192.0.2.1:5000"

	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on Webhook limit breach, got %d", w3.Code)
	}
	if w3.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

func TestHTTP_Webhook(t *testing.T) {
	handler, _, secret, jwtSecret, _ := setupTestServer(t)
	token := issueToken(t, jwtSecret, 1)

	createReq := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Idempotency-Key", "webhook-http-key-12345")
	createReq.Header.Set("Authorization", "Bearer "+token)
	wCreate := httptest.NewRecorder()
	handler.ServeHTTP(wCreate, createReq)

	var created struct {
		PaymentID int64 `json:"payment_id"`
	}
	if err := json.Unmarshal(wCreate.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created payment: %v", err)
	}

	webhookPayload := []byte(fmt.Sprintf(`{
		"event_id": "evt_http_001",
		"event_type": "payment.succeeded",
		"payment_id": %d,
		"provider_payment_id": "ch_test_123",
		"amount_minor": 50000,
		"currency": "KZT"
	}`, created.PaymentID))

	t.Run("valid_webhook_200", func(t *testing.T) {
		now := time.Now().Unix()
		sig := sign(secret, now, webhookPayload)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid_signature_401_no_detail_leak", func(t *testing.T) {
		now := time.Now().Unix()
		fakeSig := "badbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadb"

		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", fakeSig)
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized, got %d", w.Code)
		}
		expectedBody := "{\"error\":{\"code\":\"unauthorized\",\"message\":\"Unauthorized\"}}\n"
		if w.Body.String() != expectedBody {
			t.Errorf("expected clean 401 error envelope, got: %s", w.Body.String())
		}
	})

	t.Run("body_exceeds_64kb_rejected_400", func(t *testing.T) {
		largeBody := make([]byte, 65*1024)
		copy(largeBody, webhookPayload)
		now := time.Now().Unix()
		sig := sign(secret, now, largeBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(largeBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for body > 64KB, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("timestamp_past_301s_rejected_401", func(t *testing.T) {
		pastTime := time.Now().Unix() - 301
		sig := sign(secret, pastTime, webhookPayload)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", pastTime))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for timestamp older than 5 minutes, got %d", w.Code)
		}
	})

	t.Run("timestamp_future_301s_rejected_401", func(t *testing.T) {
		futureTime := time.Now().Unix() + 301
		sig := sign(secret, futureTime, webhookPayload)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", futureTime))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for timestamp in future > 5 minutes, got %d", w.Code)
		}
	})

	t.Run("missing_security_headers_rejected_401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for missing headers, got %d", w.Code)
		}
	})

	t.Run("malformed_hex_signature_rejected_401", func(t *testing.T) {
		now := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(webhookPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", "not-a-valid-hex-signature-string")
		req.Header.Set("X-Timestamp", fmt.Sprintf("%d", now))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for malformed hex signature, got %d", w.Code)
		}
	})
}

func TestHTTP_Healthz_And_Readyz(t *testing.T) {
	t.Run("healthz_always_alive", func(t *testing.T) {
		handler, _, _, _, _ := setupTestServer(t)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK from /healthz, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "alive") {
			t.Fatalf("expected response containing alive, got %s", w.Body.String())
		}
	})

	t.Run("readyz_ready_when_checker_succeeds", func(t *testing.T) {
		readyChecker := func(ctx context.Context) error {
			return nil
		}
		handler, _, _, _, _ := setupTestServer(t, httpapi.WithReadyChecker(readyChecker))
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK from /readyz, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "ready") {
			t.Fatalf("expected response containing ready, got %s", w.Body.String())
		}
	})

	t.Run("readyz_503_when_checker_fails", func(t *testing.T) {
		readyChecker := func(ctx context.Context) error {
			return errors.New("db connection ping failed")
		}
		handler, _, _, _, _ := setupTestServer(t, httpapi.WithReadyChecker(readyChecker))
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable from /readyz, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "not_ready") {
			t.Fatalf("expected response containing not_ready, got %s", w.Body.String())
		}
	})
}

func TestHTTP_MetricsEndpoint(t *testing.T) {
	handler, _, _, _, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /metrics, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "miniwallet_payments_total") && !strings.Contains(body, "go_goroutines") {
		t.Fatalf("expected prometheus metrics format in /metrics output, got: %s", body)
	}
}
