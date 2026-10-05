package httpapi_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/httpapi"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

// TestRateLimiter_MemoryCleanup проверяет, что неактивные бакеты удаляются,
// предотвращая неограниченный рост памяти при большом количестве уникальных ключей.
func TestRateLimiter_MemoryCleanup(t *testing.T) {
	mockTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := &testClock{current: mockTime}

	rl := httpapi.NewRateLimiter(10, 20, clock)

	// Создаем 100 уникальных ключей
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("ip:192.168.1.%d", i)
		allowed, _ := rl.Allow(key)
		if !allowed {
			t.Fatalf("expected key %s to be allowed", key)
		}
	}

	if rl.Len() != 100 {
		t.Fatalf("expected 100 buckets, got %d", rl.Len())
	}

	// Сдвигаем время на 10 минут вперед (больше idle TTL)
	clock.current = clock.current.Add(10 * time.Minute)

	// Новый запрос должен запустить автоматическую очистку устаревших бакетов
	allowed, _ := rl.Allow("ip:10.0.0.1")
	if !allowed {
		t.Fatalf("expected new key to be allowed")
	}

	// Все 100 старых бакетов должны быть удалены, остался только 1 новый
	if rl.Len() != 1 {
		t.Fatalf("expected 1 bucket after cleanup of stale keys, got %d (memory leak!)", rl.Len())
	}
}

// TestClientIP_ProxyAndAntiSpoofing проверяет:
// 1. Прямое недоверенное соединение не может подделать IP через заголовок X-Forwarded-For.
// 2. За доверенным прокси IP определяется безопасно (справа налево, пропуская подделку клиента).
func TestClientIP_ProxyAndAntiSpoofing(t *testing.T) {
	t.Run("direct_untrusted_connection_ignores_xff", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/payments", nil)
		req.RemoteAddr = "203.0.113.195:12345"
		req.Header.Set("X-Forwarded-For", "1.1.1.1, 8.8.8.8")

		ip := httpapi.ClientIP(req)
		if ip != "203.0.113.195" {
			t.Fatalf("expected direct untrusted connection to use RemoteAddr 203.0.113.195, got %s (IP spoofing vulnerability!)", ip)
		}
	})

	t.Run("behind_trusted_proxy_parses_right_to_left", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/payments", nil)
		// Соединение пришло с локального Nginx/Envoy (доверенный loopback прокси)
		req.RemoteAddr = "127.0.0.1:54321"
		// Клиент отправил X-Forwarded-For: 8.8.8.8, а прокси добавил реальный IP клиента 203.0.113.50 в конец
		req.Header.Set("X-Forwarded-For", "8.8.8.8, 203.0.113.50")

		ip := httpapi.ClientIP(req)
		if ip != "203.0.113.50" {
			t.Fatalf("expected rightmost client IP from trusted proxy 203.0.113.50, got %s (leftmost spoofed IP was taken!)", ip)
		}
	})
}

// TestHTTP_WebhookNotBlockedByUserRateLimit проверяет, что исчерпание лимита пользователя
// на эндпоинте /payments никак не влияет на обработку вебхуков от провайдера на /webhooks/provider.
func TestHTTP_WebhookNotBlockedByUserRateLimit(t *testing.T) {
	store := memory.New()
	prov := &dummyProvider{}
	secret := "wh-secret-1234567890123456789012345"
	jwtSecret := "jwt-secret-1234567890123456789012345"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := service.NewPaymentService(
		store.Users(), store.Orders(), store.Payments(),
		store.Webhooks(), store.PaymentEvents(), store.SecurityEvents(),
		store, prov, logger, secret,
	)

	// Жесткий лимит на пользователя: 1 запрос (burst 1)
	userLimiter := httpapi.NewRateLimiter(1, 1, domain.RealClock{})
	// Жесткий лимит на IP платежей: 1 запрос (burst 1)
	ipLimiter := httpapi.NewRateLimiter(1, 1, domain.RealClock{})
	// Лимит на вебхуки свободный
	whLimiter := httpapi.NewRateLimiter(50, 100, domain.RealClock{})

	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithPaymentsUserLimiter(userLimiter),
		httpapi.WithPaymentsIPLimiter(ipLimiter),
		httpapi.WithWebhookLimiter(whLimiter),
	)

	store.SeedUser(&domain.User{ID: 1, Email: "user@kz", IsActive: true})
	store.SeedOrder(&domain.Order{ID: 10, UserID: 1, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
	store.SeedOrder(&domain.Order{ID: 11, UserID: 1, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

	token := issueToken(t, jwtSecret, 1)
	clientIP := "198.51.100.25:12345"

	// 1. Первый запрос на /payments проходит
	req1 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":10}`)))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "idem-user-key-0001")
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.RemoteAddr = clientIP
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected request 1 to pass, got %d", w1.Code)
	}

	// 2. Второй запрос на /payments блокируется 429 Too Many Requests (лимиты пользователя и IP исчерпаны)
	req2 := httptest.NewRequest(http.MethodPost, "/payments", bytes.NewReader([]byte(`{"order_id":11}`)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "idem-user-key-0002")
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.RemoteAddr = clientIP
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on payments endpoint, got %d", w2.Code)
	}

	// 3. Вебхук от провайдера для этого же заказа/платежа с того же IP ОБЯЗАН пройти успешно (200 OK)
	// и НЕ блокироваться исчерпанными лимитами пользователя или пользовательского IP!
	payload := []byte(`{"event_id":"evt_wh_user_exhausted","event_type":"payment.succeeded","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"ch_test_123"}`)
	nowUnix := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", nowUnix)))
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))

	reqWH := httptest.NewRequest(http.MethodPost, "/webhooks/provider", bytes.NewReader(payload))
	reqWH.Header.Set("Content-Type", "application/json")
	reqWH.Header.Set("X-Signature", sig)
	reqWH.Header.Set("X-Timestamp", fmt.Sprintf("%d", nowUnix))
	reqWH.RemoteAddr = clientIP
	wWH := httptest.NewRecorder()
	handler.ServeHTTP(wWH, reqWH)

	if wWH.Code != http.StatusOK {
		t.Fatalf("expected webhook to succeed with 200 OK even when user/IP limit on /payments is exhausted, got %d: %s", wWH.Code, wWH.Body.String())
	}
}

type testClock struct {
	current time.Time
}

func (c *testClock) Now() time.Time {
	return c.current.UTC()
}
