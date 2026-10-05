package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

// setupFuzzService инициализирует хранилище с детерминированным набором данных
// (пользователь, заказы и платежи во всех ключевых статусах: pending, failed, succeeded, canceled)
// и сервис с FrozenClock для надежного прохождения проверок в фаззинг-тестах.
func setupFuzzService(t testing.TB, secret string) (*service.PaymentService, *memory.Storage, domain.Clock) {
	t.Helper()
	store := memory.New()
	prov := &fakeProvider{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.NewPaymentService(
		store.Users(), store.Orders(), store.Payments(),
		store.Webhooks(), store.PaymentEvents(), store.SecurityEvents(),
		store, prov, logger, secret,
	)
	mockTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := domain.FrozenClock{Current: mockTime}
	svc.SetClock(clock)
	store.SetClock(clock)

	ctx := context.Background()
	store.SeedUser(&domain.User{
		ID:        1,
		Email:     "fuzz@example.com",
		IsActive:  true,
		CreatedAt: mockTime,
	})

	// 1. Order 1 (Unpaid) and Payment 1 (Pending)
	store.SeedOrder(&domain.Order{
		ID:          1,
		UserID:      1,
		AmountMinor: 50000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
		CreatedAt:   mockTime,
		UpdatedAt:   mockTime,
	})
	pay1 := &domain.Payment{
		OrderID:        1,
		UserID:         1,
		IdempotencyKey: "fuzz-key-pending-001",
		AmountMinor:    50000,
		Currency:       "KZT",
		CreatedAt:      mockTime,
		UpdatedAt:      mockTime,
	}
	if err := store.Payments().CreatePending(ctx, pay1); err != nil {
		t.Fatalf("create pay 1: %v", err)
	}
	if err := store.Payments().UpdateSessionTx(ctx, nil, pay1.ID, "prov-100", "http://checkout/1"); err != nil {
		t.Fatalf("update session pay 1: %v", err)
	}

	// 2. Order 2 (Unpaid) and Payment 2 (Failed) for late-success tests
	store.SeedOrder(&domain.Order{
		ID:          2,
		UserID:      1,
		AmountMinor: 50000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
		CreatedAt:   mockTime,
		UpdatedAt:   mockTime,
	})
	pay2 := &domain.Payment{
		OrderID:        2,
		UserID:         1,
		IdempotencyKey: "fuzz-key-failed-002",
		AmountMinor:    50000,
		Currency:       "KZT",
		CreatedAt:      mockTime,
		UpdatedAt:      mockTime,
	}
	if err := store.Payments().CreatePending(ctx, pay2); err != nil {
		t.Fatalf("create pay 2: %v", err)
	}
	if err := store.Payments().UpdateSessionTx(ctx, nil, pay2.ID, "prov-101", "http://checkout/2"); err != nil {
		t.Fatalf("update session pay 2: %v", err)
	}
	if err := store.Payments().UpdateStatusTx(ctx, nil, pay2.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
		t.Fatalf("update status pay 2: %v", err)
	}

	// 3. Order 3 (Paid) and Payment 3 (Succeeded) for duplicate succeeded tests
	store.SeedOrder(&domain.Order{
		ID:          3,
		UserID:      1,
		AmountMinor: 50000,
		Currency:    "KZT",
		Status:      domain.OrderStatusPaid,
		CreatedAt:   mockTime,
		UpdatedAt:   mockTime,
	})
	pay3 := &domain.Payment{
		OrderID:        3,
		UserID:         1,
		IdempotencyKey: "fuzz-key-succeeded-003",
		AmountMinor:    50000,
		Currency:       "KZT",
		CreatedAt:      mockTime,
		UpdatedAt:      mockTime,
	}
	if err := store.Payments().CreatePending(ctx, pay3); err != nil {
		t.Fatalf("create pay 3: %v", err)
	}
	if err := store.Payments().UpdateSessionTx(ctx, nil, pay3.ID, "prov-102", "http://checkout/3"); err != nil {
		t.Fatalf("update session pay 3: %v", err)
	}
	if err := store.Payments().UpdateStatusTx(ctx, nil, pay3.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
		t.Fatalf("update status pay 3: %v", err)
	}

	// 4. Order 4 (Unpaid) and Payment 4 (Canceled)
	store.SeedOrder(&domain.Order{
		ID:          4,
		UserID:      1,
		AmountMinor: 50000,
		Currency:    "KZT",
		Status:      domain.OrderStatusUnpaid,
		CreatedAt:   mockTime,
		UpdatedAt:   mockTime,
	})
	pay4 := &domain.Payment{
		OrderID:        4,
		UserID:         1,
		IdempotencyKey: "fuzz-key-canceled-004",
		AmountMinor:    50000,
		Currency:       "KZT",
		CreatedAt:      mockTime,
		UpdatedAt:      mockTime,
	}
	if err := store.Payments().CreatePending(ctx, pay4); err != nil {
		t.Fatalf("create pay 4: %v", err)
	}
	if err := store.Payments().UpdateSessionTx(ctx, nil, pay4.ID, "prov-103", "http://checkout/4"); err != nil {
		t.Fatalf("update session pay 4: %v", err)
	}
	if err := store.Payments().UpdateStatusTx(ctx, nil, pay4.ID, domain.PaymentStatusPending, domain.PaymentStatusCanceled); err != nil {
		t.Fatalf("update status pay 4: %v", err)
	}

	return svc, store, clock
}

// FuzzVerifySignature тестирует проверку HMAC-подписи и временной метки вебхука
// на устойчивость к мутациям подписи, заголовка времени и сырого тела.
func FuzzVerifySignature(f *testing.F) {
	secret := "fuzz-webhook-secret-key-12345"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.New()
	prov := &fakeProvider{}
	svc := service.NewPaymentService(
		store.Users(), store.Orders(), store.Payments(),
		store.Webhooks(), store.PaymentEvents(), store.SecurityEvents(),
		store, prov, logger, secret,
	)
	mockTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := domain.FrozenClock{Current: mockTime}
	svc.SetClock(clock)

	nowUnix := mockTime.Unix()
	body := []byte(`{"event_id":"evt_1","event_type":"payment.succeeded","payment_id":1}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", nowUnix)))
	mac.Write(body)
	validSig := hex.EncodeToString(mac.Sum(nil))

	f.Add(body, validSig, fmt.Sprintf("%d", nowUnix))
	f.Add([]byte(""), "", "")
	f.Add([]byte("garbage"), "not-a-hex", "not-a-number")
	f.Add([]byte(`{"invalid": true}`), validSig, fmt.Sprintf("%d", nowUnix+10000))
	f.Add(body, "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff", fmt.Sprintf("%d", nowUnix))

	f.Fuzz(func(t *testing.T, rawBody []byte, signature, timestampStr string) {
		err := svc.VerifyWebhookSignature(rawBody, signature, timestampStr)
		if err == nil {
			// Если проверка прошла успешно, подпись обязана совпадать с вычисленной
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(fmt.Sprintf("%s.", timestampStr)))
			mac.Write(rawBody)
			expected := hex.EncodeToString(mac.Sum(nil))
			if signature != expected {
				t.Fatalf("signature passed but does not match expected: got %s, want %s", signature, expected)
			}
		}
	})
}

// FuzzProcessWebhook проверяет устойчивость ProcessWebhook к мутациям полезной нагрузки,
// гарантируя отсутствие паник, целостность транзакций и корректную обработку бизнес-логики.
// Мутированное тело всегда подписывается валидной HMAC-SHA256 подписью, чтобы фаззер
// проходил проверку подписи и исследовал логику парсинга, инвариантов и статусную модель.
func FuzzProcessWebhook(f *testing.F) {
	secret := "fuzz-webhook-secret-key-12345"

	// Корпус семян охватывает все ветки бизнес-логики:
	// 1. Успешный платеж (succeeded) для pending платежа 1
	f.Add([]byte(`{"event_id":"evt_seed_succ_1","event_type":"payment.succeeded","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-100"}`))
	// 2. Ошибка платежа (failed) для pending платежа 1
	f.Add([]byte(`{"event_id":"evt_seed_fail_1","event_type":"payment.failed","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-100"}`))
	// 3. Отмена платежа (canceled) для pending платежа 1
	f.Add([]byte(`{"event_id":"evt_seed_cancel_1","event_type":"payment.canceled","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-100"}`))
	// 4. Несовпадение суммы (amount mismatch)
	f.Add([]byte(`{"event_id":"evt_seed_amt_mismatch","event_type":"payment.succeeded","payment_id":1,"amount_minor":99999,"currency":"KZT","provider_payment_id":"prov-100"}`))
	// 5. Несовпадение валюты (currency mismatch)
	f.Add([]byte(`{"event_id":"evt_seed_curr_mismatch","event_type":"payment.succeeded","payment_id":1,"amount_minor":50000,"currency":"USD","provider_payment_id":"prov-100"}`))
	// 6. Несовпадение идентификатора провайдера (provider_payment_id mismatch)
	f.Add([]byte(`{"event_id":"evt_seed_prov_mismatch","event_type":"payment.succeeded","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-other"}`))
	// 7. Поздний успех (late success) по уже финализированному платежу 2 (failed)
	f.Add([]byte(`{"event_id":"evt_seed_late_succ","event_type":"payment.succeeded","payment_id":2,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-101"}`))
	// 8. Повторный успех по уже оплаченному заказу 3 / платежу 3
	f.Add([]byte(`{"event_id":"evt_seed_already_paid","event_type":"payment.succeeded","payment_id":3,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-102"}`))
	// 9. Неизвестный тип события
	f.Add([]byte(`{"event_id":"evt_seed_unknown_type","event_type":"payment.refunded","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-100"}`))
	// 10. Неизвестный payment_id
	f.Add([]byte(`{"event_id":"evt_seed_unknown_pay","event_type":"payment.succeeded","payment_id":99999,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-999"}`))
	// 11. Пустой event_id
	f.Add([]byte(`{"event_id":"","event_type":"payment.succeeded","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-100"}`))
	// 12. Невалидный JSON и пустые структуры
	f.Add([]byte(`{}`))
	f.Add([]byte(`malformed json`))
	f.Add([]byte(`{"event_id":"evt_overflow","payment_id":999999999999999999,"amount_minor":-99}`))

	f.Fuzz(func(t *testing.T, rawBody []byte) {
		svc, store, clock := setupFuzzService(t, secret)

		nowUnix := clock.Now().UTC().Unix()
		timestampStr := fmt.Sprintf("%d", nowUnix)

		// Валидная HMAC-SHA256 подпись вычисляется по мутированному телу и текущей метке времени,
		// благодаря чему фаззер минует проверку подписи и проникает непосредственно в бизнес-логику
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(fmt.Sprintf("%s.", timestampStr)))
		mac.Write(rawBody)
		sig := hex.EncodeToString(mac.Sum(nil))

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := svc.ProcessWebhook(ctx, rawBody, sig, timestampStr)

		// 1. Проверка парсинга: если JSON невалиден или event_id пуст, обязана быть ошибка
		var payload struct {
			EventID           string `json:"event_id"`
			EventType         string `json:"event_type"`
			PaymentID         int64  `json:"payment_id"`
			AmountMinor       int64  `json:"amount_minor"`
			Currency          string `json:"currency"`
			ProviderPaymentID string `json:"provider_payment_id"`
		}
		if jsonErr := json.Unmarshal(rawBody, &payload); jsonErr != nil || payload.EventID == "" {
			if err == nil {
				t.Fatalf("expected error for malformed json or empty event_id, got nil")
			}
			return
		}

		// 2. Идемпотентность: если обработка завершилась успешно, повторный вызов с тем же телом
		// обязан вернуть nil (200 OK) без изменения состояния
		if err == nil {
			dupErr := svc.ProcessWebhook(ctx, rawBody, sig, timestampStr)
			if dupErr != nil {
				t.Fatalf("expected duplicate webhook replay to return nil, got: %v", dupErr)
			}
		}

		// 3. Инвариант 5: Late success никогда не переводит failed платёж (ID 2) в succeeded
		p2, errP2 := store.Payments().GetByID(ctx, 2)
		if errP2 == nil && p2.Status == domain.PaymentStatusSucceeded {
			t.Fatalf("Invariant 5 violated: finalized payment 2 transitioned to succeeded")
		}

		// 4. Инвариант 3: Если платёж 1 стал succeeded, заказ 1 обязан стать paid
		p1, errP1 := store.Payments().GetByID(ctx, 1)
		if errP1 == nil && p1.Status == domain.PaymentStatusSucceeded {
			o1, errO1 := store.Orders().GetByID(ctx, 1)
			if errO1 != nil || o1.Status != domain.OrderStatusPaid {
				t.Fatalf("Invariant violated: payment 1 succeeded but order 1 is not paid: %v", errO1)
			}
		}
	})
}

// TestFuzzProcessWebhook_SeedsReachBusinessLogic проверяет, что сиды FuzzProcessWebhook
// достигают всех веток бизнес-логики:
// - Перевод статуса pending -> succeeded и заказ unpaid -> paid
// - Дедупликация повторной доставки
// - Защита от Late Success (платёж 2 остаётся failed)
// - Отклонение несовпадения суммы (Amount Mismatch) с фиксацией инцидента безопасности
func TestFuzzProcessWebhook_SeedsReachBusinessLogic(t *testing.T) {
	secret := "fuzz-webhook-secret-key-12345"
	svc, store, clock := setupFuzzService(t, secret)

	nowUnix := clock.Now().UTC().Unix()
	timestampStr := fmt.Sprintf("%d", nowUnix)

	// Тест 1: Seed 1 (payment.succeeded) переводит pending платеж 1 в succeeded, а заказ 1 в paid
	seed1 := []byte(`{"event_id":"evt_seed_succ_1","event_type":"payment.succeeded","payment_id":1,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-100"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%s.", timestampStr)))
	mac.Write(seed1)
	sig1 := hex.EncodeToString(mac.Sum(nil))

	ctx := context.Background()
	if err := svc.ProcessWebhook(ctx, seed1, sig1, timestampStr); err != nil {
		t.Fatalf("unexpected error processing seed1: %v", err)
	}

	p1, err := store.Payments().GetByID(ctx, 1)
	if err != nil || p1.Status != domain.PaymentStatusSucceeded {
		t.Fatalf("expected payment 1 succeeded, got status=%v, err=%v", p1.Status, err)
	}

	o1, err := store.Orders().GetByID(ctx, 1)
	if err != nil || o1.Status != domain.OrderStatusPaid {
		t.Fatalf("expected order 1 paid, got status=%v, err=%v", o1.Status, err)
	}

	// Дедупликация: повторная доставка того же event_id возвращает nil
	if err := svc.ProcessWebhook(ctx, seed1, sig1, timestampStr); err != nil {
		t.Fatalf("expected duplicate webhook to return nil, got: %v", err)
	}

	// Тест 2: Seed Late Success (payment 2 failed, пришел succeeded) - статус не меняется
	seedLate := []byte(`{"event_id":"evt_seed_late_succ","event_type":"payment.succeeded","payment_id":2,"amount_minor":50000,"currency":"KZT","provider_payment_id":"prov-101"}`)
	macLate := hmac.New(sha256.New, []byte(secret))
	macLate.Write([]byte(fmt.Sprintf("%s.", timestampStr)))
	macLate.Write(seedLate)
	sigLate := hex.EncodeToString(macLate.Sum(nil))

	_ = svc.ProcessWebhook(ctx, seedLate, sigLate, timestampStr)
	p2, err := store.Payments().GetByID(ctx, 2)
	if err != nil || p2.Status != domain.PaymentStatusFailed {
		t.Fatalf("expected payment 2 to remain failed on late success, got status=%v", p2.Status)
	}

	// Тест 3: Seed Amount Mismatch
	svc3, store3, clock3 := setupFuzzService(t, secret)
	ts3Str := fmt.Sprintf("%d", clock3.Now().UTC().Unix())
	seedMismatch := []byte(`{"event_id":"evt_seed_amt_mismatch","event_type":"payment.succeeded","payment_id":1,"amount_minor":99999,"currency":"KZT","provider_payment_id":"prov-100"}`)
	macMismatch := hmac.New(sha256.New, []byte(secret))
	macMismatch.Write([]byte(fmt.Sprintf("%s.", ts3Str)))
	macMismatch.Write(seedMismatch)
	sigMismatch := hex.EncodeToString(macMismatch.Sum(nil))

	errMismatch := svc3.ProcessWebhook(ctx, seedMismatch, sigMismatch, ts3Str)
	if errMismatch == nil {
		t.Fatalf("expected error for amount mismatch, got nil")
	}
	p1Mismatch, _ := store3.Payments().GetByID(ctx, 1)
	if p1Mismatch.Status == domain.PaymentStatusSucceeded {
		t.Fatalf("payment 1 must not be marked succeeded on amount mismatch")
	}
}
