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

	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

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

	nowUnix := time.Now().Unix()
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
// гарантируя отсутствие паник и корректную обработку невалидного JSON и граничных случаев.
func FuzzProcessWebhook(f *testing.F) {
	secret := "fuzz-webhook-secret-key-12345"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	f.Add([]byte(`{"event_id":"evt_seed_1","event_type":"payment.succeeded","payment_id":100,"amount_minor":50000,"currency":"KZT"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`malformed json`))
	f.Add([]byte(`{"event_id":"","event_type":"","payment_id":-1}`))
	f.Add([]byte(`{"event_id":"evt_overflow","payment_id":999999999999999999,"amount_minor":-99}`))

	f.Fuzz(func(t *testing.T, rawBody []byte) {
		store := memory.New()
		prov := &fakeProvider{}
		svc := service.NewPaymentService(
			store.Users(), store.Orders(), store.Payments(),
			store.Webhooks(), store.PaymentEvents(), store.SecurityEvents(),
			store, prov, logger, secret,
		)

		nowUnix := time.Now().Unix()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(fmt.Sprintf("%d.", nowUnix)))
		mac.Write(rawBody)
		sig := hex.EncodeToString(mac.Sum(nil))

		// ProcessWebhook не должен паниковать при любых входных данных
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := svc.ProcessWebhook(ctx, rawBody, sig, fmt.Sprintf("%d", nowUnix))
		// Если json невалиден, должна возвращаться ошибка парсинга или валидации
		var payload struct {
			EventID string `json:"event_id"`
		}
		if jsonErr := json.Unmarshal(rawBody, &payload); jsonErr != nil || payload.EventID == "" {
			if err == nil {
				t.Fatalf("expected error for malformed json or empty event_id, got nil")
			}
		}
	})
}
