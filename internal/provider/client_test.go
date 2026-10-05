package provider_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/provider"
)

func TestClient_Retry_503Then200(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		if count < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte(`{"error":"temporarily_unavailable"}`)); err != nil {
				t.Logf("write response error: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"provider_payment_id":"ch_123","checkout_url":"https://checkout.fake/pay/123"}`)); err != nil {
			t.Logf("write response error: %v", err)
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		500*time.Millisecond,
		4,
		5*time.Millisecond,
		20*time.Millisecond,
	)

	session, err := client.CreateCheckoutSession(context.Background(), provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "test-idem-key-12345",
	})
	if err != nil {
		t.Fatalf("expected success on 3rd attempt, got error: %v", err)
	}

	if session.ProviderPaymentID != "ch_123" {
		t.Errorf("expected provider_payment_id 'ch_123', got '%s'", session.ProviderPaymentID)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 server calls, got %d", calls.Load())
	}
}

func TestClient_Retry_400NotRetried(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":"bad_request"}`)); err != nil {
			t.Logf("write response error: %v", err)
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		500*time.Millisecond,
		4,
		5*time.Millisecond,
		20*time.Millisecond,
	)

	_, err := client.CreateCheckoutSession(context.Background(), provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "test-idem-key-12345",
	})
	if err == nil {
		t.Fatal("expected error on 400 Bad Request, got nil")
	}

	if !errors.Is(err, provider.ErrProviderClientError) {
		t.Errorf("expected ErrProviderClientError, got %v", err)
	}
	if !provider.IsDefinitiveRejection(err) {
		t.Errorf("expected IsDefinitiveRejection to be true, got false for err: %v", err)
	}
	if provider.IsAmbiguousOutcome(err) {
		t.Errorf("expected IsAmbiguousOutcome to be false for 400, got true")
	}
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 call (no retries for 400), got %d", calls.Load())
	}
}

func TestClient_Retry_ContextCanceled(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		500*time.Millisecond,
		4,
		100*time.Millisecond,
		500*time.Millisecond,
	)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := client.CreateCheckoutSession(ctx, provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "test-idem-key-12345",
	})
	if err == nil {
		t.Fatal("expected context canceled error, got nil")
	}

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestClient_Retry_500ExhaustedIsAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		200*time.Millisecond,
		2,
		5*time.Millisecond,
		10*time.Millisecond,
	)

	_, err := client.CreateCheckoutSession(context.Background(), provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "test-idem-key-exhausted",
	})
	if err == nil {
		t.Fatal("expected error on 500 Internal Server Error, got nil")
	}
	if !provider.IsAmbiguousOutcome(err) {
		t.Errorf("expected IsAmbiguousOutcome to be true, got false for err: %v", err)
	}
	if provider.IsDefinitiveRejection(err) {
		t.Errorf("expected IsDefinitiveRejection to be false for 500, got true")
	}
}

func TestProvider_SemaphoreLimit(t *testing.T) {
	blockCh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"provider_payment_id":"ch_sem","checkout_url":"https://checkout.fake/pay/sem"}`)); err != nil {
			t.Logf("write response error: %v", err)
		}
	}))
	defer server.Close()
	defer close(blockCh)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		2*time.Second,
		1,
		5*time.Millisecond,
		10*time.Millisecond,
	)

	// Ограничиваем семафор строго 1 параллельным вызовом
	client.SetSemaphore(provider.NewSemaphore(1))

	// Запускаем первый вызов, который занимает единственный слот семафора
	firstStarted := make(chan struct{})
	go func() {
		close(firstStarted)
		sess, err := client.CreateCheckoutSession(context.Background(), provider.CreateCheckoutInput{
			PaymentID:      1,
			OrderID:        10,
			AmountMinor:    1000,
			Currency:       "KZT",
			IdempotencyKey: "sem-key-1",
		})
		if err != nil {
			t.Logf("first request returned err: %v", err)
		} else if sess.ProviderPaymentID == "" {
			t.Logf("empty session ID")
		}
	}()

	<-firstStarted
	// Небольшая задержка, чтобы горутина успела захватить слот семафора
	time.Sleep(20 * time.Millisecond)

	// Второй вызов должен быть отклонён семафором с ErrProviderOverloaded
	_, err := client.CreateCheckoutSession(context.Background(), provider.CreateCheckoutInput{
		PaymentID:      2,
		OrderID:        11,
		AmountMinor:    2000,
		Currency:       "KZT",
		IdempotencyKey: "sem-key-2",
	})
	if !errors.Is(err, provider.ErrProviderOverloaded) {
		t.Fatalf("expected ErrProviderOverloaded, got: %v", err)
	}
}

func TestProvider_CircuitBreaker_Transitions(t *testing.T) {
	var shouldFail atomic.Bool
	shouldFail.Store(true)
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if shouldFail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"provider_payment_id":"ch_cb","checkout_url":"https://checkout.fake/pay/cb"}`)); err != nil {
			t.Logf("write response error: %v", err)
		}
	}))
	defer server.Close()

	baseTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	frozenClock := &domain.FrozenClock{Current: baseTime}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		200*time.Millisecond,
		1, // Без ретраев для проверки счетчика цепи
		5*time.Millisecond,
		10*time.Millisecond,
	)

	// Порог 2 ошибки, cooldown 10 секунд
	cb := provider.NewCircuitBreaker(2, 10*time.Second, 1, frozenClock)
	client.SetCircuitBreaker(cb)

	input := provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "cb-key-1",
	}

	// 1. Первая ошибка -> StateClosed
	_, err1 := client.CreateCheckoutSession(context.Background(), input)
	if err1 == nil {
		t.Fatal("expected failure on call 1")
	}
	if cb.State() != provider.StateClosed {
		t.Fatalf("expected StateClosed after 1 failure, got %v", cb.State())
	}

	// 2. Вторая ошибка -> переход в StateOpen
	_, err2 := client.CreateCheckoutSession(context.Background(), input)
	if err2 == nil {
		t.Fatal("expected failure on call 2")
	}
	if cb.State() != provider.StateOpen {
		t.Fatalf("expected StateOpen after 2 failures, got %v", cb.State())
	}

	callsBeforeOpenCheck := callCount.Load()

	// 3. Вызов при StateOpen -> немедленный возврат ErrCircuitOpen без сетевого запроса
	_, err3 := client.CreateCheckoutSession(context.Background(), input)
	if !errors.Is(err3, provider.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen when circuit is open, got: %v", err3)
	}
	if callCount.Load() != callsBeforeOpenCheck {
		t.Fatalf("circuit breaker must NOT make network calls while Open, calls changed from %d to %d",
			callsBeforeOpenCheck, callCount.Load())
	}

	// 4. Прошло 5 секунд (меньше cooldown 10 сек) -> всё еще Open
	frozenClock.Current = baseTime.Add(5 * time.Second)
	_, err4 := client.CreateCheckoutSession(context.Background(), input)
	if !errors.Is(err4, provider.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen before cooldown expired, got: %v", err4)
	}

	// 5. Прошло 11 секунд (cooldown истёк) -> переход в HalfOpen для пробного запроса
	frozenClock.Current = baseTime.Add(11 * time.Second)
	shouldFail.Store(false) // Сервер восстановился

	session, err5 := client.CreateCheckoutSession(context.Background(), input)
	if err5 != nil {
		t.Fatalf("canary request in HalfOpen failed: %v", err5)
	}
	if session.ProviderPaymentID != "ch_cb" {
		t.Fatalf("expected ch_cb, got %s", session.ProviderPaymentID)
	}

	// После успешного пробного запроса цепь переходит обратно в StateClosed
	if cb.State() != provider.StateClosed {
		t.Fatalf("expected StateClosed after successful canary, got %v", cb.State())
	}
}

func TestProvider_RetryBudgetExhausted(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		50*time.Millisecond,
		5,                    // 5 попыток настроено
		200*time.Millisecond, // Но backoff большой (200ms)
		500*time.Millisecond,
	)

	// Контекст с таймаутом меньше, чем backoff + 50ms
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.CreateCheckoutSession(ctx, provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "budget-key-1",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Попытка должна быть ровно 1: бюджет повторов прервал цикл до сна и повтора
	if attempts.Load() != 1 {
		t.Fatalf("expected exactly 1 attempt due to exhausted retry budget, got %d", attempts.Load())
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("expected quick termination without waiting full backoff, elapsed: %v", elapsed)
	}
}

func TestClient_GetPaymentStatus_BodySizeLimitExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Send 100 KB payload (exceeding 64 KB limit) with JSON only at the very end
		bigData := make([]byte, 100*1024)
		for i := range bigData {
			bigData[i] = ' '
		}
		copy(bigData[len(bigData)-22:], []byte(`{"status":"succeeded"}`))
		_, _ = w.Write(bigData)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		500*time.Millisecond,
		1,
		5*time.Millisecond,
		20*time.Millisecond,
	)

	status, err := client.GetPaymentStatus(context.Background(), "lookup-large-1")
	if err == nil {
		t.Fatalf("expected error due to exceeded body size limit, got status: %v", status)
	}
}

func TestClient_CreateCheckoutSession_BodySizeLimitExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		bigData := make([]byte, 100*1024)
		for i := range bigData {
			bigData[i] = ' '
		}
		copy(bigData[len(bigData)-70:], []byte(`{"provider_payment_id":"ch_123","checkout_url":"https://fake/pay"}`))
		_, _ = w.Write(bigData)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := provider.NewClient(
		server.URL,
		server.Client(),
		logger,
		500*time.Millisecond,
		1,
		5*time.Millisecond,
		20*time.Millisecond,
	)

	_, err := client.CreateCheckoutSession(context.Background(), provider.CreateCheckoutInput{
		PaymentID:      1,
		OrderID:        10,
		AmountMinor:    1000,
		Currency:       "KZT",
		IdempotencyKey: "test-limit-key-1",
	})
	if err == nil {
		t.Fatal("expected error due to exceeded body size limit on CreateCheckoutSession, got nil")
	}
}
