package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/metrics"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
	"github.com/Senjumarru/miniwallet/internal/storage/sqlite"
)

type contractTestStore interface {
	Users() service.UserRepository
	Orders() service.OrderRepository
	Payments() service.PaymentRepository
	Webhooks() service.WebhookEventRepository
	PaymentEvents() service.PaymentEventRepository
	SecurityEvents() service.SecurityEventRepository
	WithinTransaction(ctx context.Context, fn func(txCtx context.Context, tx *sql.Tx) error) error
	SeedUser(u *domain.User)
	SeedOrder(o *domain.Order)
}

type sqliteStoreWrapper struct {
	*sqlite.Storage
}

func (s *sqliteStoreWrapper) Users() service.UserRepository {
	return s.Storage.Users()
}
func (s *sqliteStoreWrapper) Orders() service.OrderRepository {
	return s.Storage.Orders()
}
func (s *sqliteStoreWrapper) Payments() service.PaymentRepository {
	return s.Storage.Payments()
}
func (s *sqliteStoreWrapper) Webhooks() service.WebhookEventRepository {
	return s.Storage.Webhooks()
}
func (s *sqliteStoreWrapper) PaymentEvents() service.PaymentEventRepository {
	return s.Storage.PaymentEvents()
}
func (s *sqliteStoreWrapper) SecurityEvents() service.SecurityEventRepository {
	return s.Storage.SecurityEvents()
}

type memoryStoreWrapper struct {
	*memory.Storage
}

func (s *memoryStoreWrapper) Users() service.UserRepository {
	return s.Storage.Users()
}
func (s *memoryStoreWrapper) Orders() service.OrderRepository {
	return s.Storage.Orders()
}
func (s *memoryStoreWrapper) Payments() service.PaymentRepository {
	return s.Storage.Payments()
}
func (s *memoryStoreWrapper) Webhooks() service.WebhookEventRepository {
	return s.Storage.Webhooks()
}
func (s *memoryStoreWrapper) PaymentEvents() service.PaymentEventRepository {
	return s.Storage.PaymentEvents()
}
func (s *memoryStoreWrapper) SecurityEvents() service.SecurityEventRepository {
	return s.Storage.SecurityEvents()
}
func (s *sqliteStoreWrapper) SeedUser(u *domain.User) {
	isBlocked := 0
	if u.IsBlocked {
		isBlocked = 1
	}
	isActive := 1
	if !u.IsActive {
		isActive = 0
	}
	_, err := s.DB().Exec(`
		INSERT INTO users (id, email, is_active, is_blocked)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			email = excluded.email,
			is_active = excluded.is_active,
			is_blocked = excluded.is_blocked`,
		u.ID, u.Email, isActive, isBlocked)
	if err != nil {
		panic(fmt.Sprintf("seed user failed: %v", err))
	}
}
func (s *sqliteStoreWrapper) SeedOrder(o *domain.Order) {
	status := string(o.Status)
	if status == "" {
		status = "unpaid"
	}
	curr := o.Currency
	if curr == "" {
		curr = "KZT"
	}
	_, err := s.DB().Exec(`
		INSERT INTO orders (id, user_id, amount_minor, currency, status)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user_id = excluded.user_id,
			amount_minor = excluded.amount_minor,
			currency = excluded.currency,
			status = excluded.status`,
		o.ID, o.UserID, o.AmountMinor, curr, status)
	if err != nil {
		panic(fmt.Sprintf("seed order failed: %v", err))
	}
}

type storeFactory struct {
	name  string
	setup func(t *testing.T) (contractTestStore, func())
}

func getStoreFactories(t *testing.T) []storeFactory {
	return []storeFactory{
		{
			name: "memory",
			setup: func(t *testing.T) (contractTestStore, func()) {
				s := memory.New()
				return &memoryStoreWrapper{Storage: s}, func() {}
			},
		},
		{
			name: "sqlite",
			setup: func(t *testing.T) (contractTestStore, func()) {
				tmpDir := t.TempDir()
				dbPath := filepath.Join(tmpDir, "contract.db")
				migrationsDir := filepath.Join("..", "..", "migrations")
				s, err := sqlite.Open(dbPath, migrationsDir)
				if err != nil {
					t.Fatalf("failed to open sqlite: %v", err)
				}
				return &sqliteStoreWrapper{Storage: s}, func() {
					_ = s.Close()
				}
			},
		},
	}
}

func signContractWebhook(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", timestamp)))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func setupContractService(t *testing.T, store contractTestStore) (*service.PaymentService, *fakeProvider, *metrics.Metrics, string) {
	prov := &fakeProvider{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	secret := "contract-webhook-secret-32bytes-secure!"
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
		secret,
	)
	svc.SetMetrics(promMetrics)
	return svc, prov, promMetrics, secret
}

// 1. ProcessWebhook: Succeeded, Failed, Canceled
func TestContract_ProcessWebhook_Statuses(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("succeeded", func(t *testing.T) {
				store, teardown := factory.setup(t)
				defer teardown()
				svc, _, _, secret := setupContractService(t, store)

				store.SeedUser(&domain.User{ID: 1, Email: "u1@kz", IsActive: true})
				store.SeedOrder(&domain.Order{ID: 10, UserID: 1, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

				payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         1,
					OrderID:        10,
					IdempotencyKey: "idem-key-succeeded-11",
				})
				if err != nil {
					t.Fatalf("create payment: %v", err)
				}

				payload, _ := json.Marshal(service.WebhookPayload{
					EventID:           "evt_succ_1",
					EventType:         "payment.succeeded",
					PaymentID:         payRes.Payment.ID,
					ProviderPaymentID: payRes.Payment.ProviderPaymentID,
					AmountMinor:       50000,
					Currency:          "KZT",
				})
				now := time.Now().Unix()
				sig := signContractWebhook(secret, now, payload)

				if err := svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now)); err != nil {
					t.Fatalf("process webhook: %v", err)
				}

				p, err := store.Payments().GetByID(ctx, payRes.Payment.ID)
				if err != nil || p.Status != domain.PaymentStatusSucceeded {
					t.Fatalf("expected payment succeeded, got %v (err=%v)", p.Status, err)
				}

				o, err := store.Orders().GetByID(ctx, 10)
				if err != nil || o.Status != domain.OrderStatusPaid {
					t.Fatalf("expected order paid, got %v (err=%v)", o.Status, err)
				}

				// Deduplication: duplicate webhook returns nil (200 OK) without side effects
				if err := svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now)); err != nil {
					t.Fatalf("expected duplicate webhook to return nil, got: %v", err)
				}
			})

			t.Run("failed", func(t *testing.T) {
				store, teardown := factory.setup(t)
				defer teardown()
				svc, _, _, secret := setupContractService(t, store)

				store.SeedUser(&domain.User{ID: 2, Email: "u2@kz", IsActive: true})
				store.SeedOrder(&domain.Order{ID: 20, UserID: 2, AmountMinor: 30000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

				payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         2,
					OrderID:        20,
					IdempotencyKey: "idem-key-failed-2222",
				})
				if err != nil {
					t.Fatalf("create payment: %v", err)
				}

				payload, _ := json.Marshal(service.WebhookPayload{
					EventID:           "evt_fail_1",
					EventType:         "payment.failed",
					PaymentID:         payRes.Payment.ID,
					ProviderPaymentID: payRes.Payment.ProviderPaymentID,
					AmountMinor:       30000,
					Currency:          "KZT",
				})
				now := time.Now().Unix()
				sig := signContractWebhook(secret, now, payload)

				if err := svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now)); err != nil {
					t.Fatalf("process webhook: %v", err)
				}

				p, err := store.Payments().GetByID(ctx, payRes.Payment.ID)
				if err != nil || p.Status != domain.PaymentStatusFailed {
					t.Fatalf("expected payment failed, got %v (err=%v)", p.Status, err)
				}

				o, err := store.Orders().GetByID(ctx, 20)
				if err != nil || o.Status != domain.OrderStatusUnpaid {
					t.Fatalf("expected order unpaid, got %v (err=%v)", o.Status, err)
				}
			})

			t.Run("canceled", func(t *testing.T) {
				store, teardown := factory.setup(t)
				defer teardown()
				svc, _, _, secret := setupContractService(t, store)

				store.SeedUser(&domain.User{ID: 3, Email: "u3@kz", IsActive: true})
				store.SeedOrder(&domain.Order{ID: 30, UserID: 3, AmountMinor: 40000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

				payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         3,
					OrderID:        30,
					IdempotencyKey: "idem-key-canceled-33",
				})
				if err != nil {
					t.Fatalf("create payment: %v", err)
				}

				payload, _ := json.Marshal(service.WebhookPayload{
					EventID:           "evt_cancel_1",
					EventType:         "payment.canceled",
					PaymentID:         payRes.Payment.ID,
					ProviderPaymentID: payRes.Payment.ProviderPaymentID,
					AmountMinor:       40000,
					Currency:          "KZT",
				})
				now := time.Now().Unix()
				sig := signContractWebhook(secret, now, payload)

				if err := svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now)); err != nil {
					t.Fatalf("process webhook: %v", err)
				}

				p, err := store.Payments().GetByID(ctx, payRes.Payment.ID)
				if err != nil || p.Status != domain.PaymentStatusCanceled {
					t.Fatalf("expected payment canceled, got %v (err=%v)", p.Status, err)
				}

				o, err := store.Orders().GetByID(ctx, 30)
				if err != nil || o.Status != domain.OrderStatusUnpaid {
					t.Fatalf("expected order unpaid, got %v (err=%v)", o.Status, err)
				}
			})
		})
	}
}

// 2. ProcessWebhook: Amount Mismatch and ProviderID Mismatch commit security events
func TestContract_ProcessWebhook_Mismatches_CommitSecurityEvents(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("amount_mismatch", func(t *testing.T) {
				store, teardown := factory.setup(t)
				defer teardown()
				svc, _, _, secret := setupContractService(t, store)

				store.SeedUser(&domain.User{ID: 4, Email: "u4@kz", IsActive: true})
				store.SeedOrder(&domain.Order{ID: 40, UserID: 4, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

				payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         4,
					OrderID:        40,
					IdempotencyKey: "idem-key-amount-mis-44",
				})
				if err != nil {
					t.Fatalf("create payment: %v", err)
				}

				payload, _ := json.Marshal(service.WebhookPayload{
					EventID:           "evt_amount_mis_1",
					EventType:         "payment.succeeded",
					PaymentID:         payRes.Payment.ID,
					ProviderPaymentID: payRes.Payment.ProviderPaymentID,
					AmountMinor:       99999, // MISMATCH
					Currency:          "KZT",
				})
				now := time.Now().Unix()
				sig := signContractWebhook(secret, now, payload)

				err = svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now))
				if !errors.Is(err, domain.ErrWebhookAmountMismatch) {
					t.Fatalf("expected ErrWebhookAmountMismatch, got: %v", err)
				}

				// Security event MUST be persisted in DB
				secEvents, err := store.SecurityEvents().ListSecurityEvents(ctx)
				if err != nil {
					t.Fatalf("list security events: %v", err)
				}
				found := false
				for _, e := range secEvents {
					if e.EventType == "security.amount_mismatch" && e.PaymentID != nil && *e.PaymentID == payRes.Payment.ID {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected security.amount_mismatch event in DB, got: %+v", secEvents)
				}

				// Payment must remain pending
				p, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
				if p.Status != domain.PaymentStatusPending {
					t.Fatalf("payment must remain pending on amount mismatch, got: %s", p.Status)
				}
			})

			t.Run("provider_id_mismatch", func(t *testing.T) {
				store, teardown := factory.setup(t)
				defer teardown()
				svc, _, _, secret := setupContractService(t, store)

				store.SeedUser(&domain.User{ID: 5, Email: "u5@kz", IsActive: true})
				store.SeedOrder(&domain.Order{ID: 50, UserID: 5, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

				payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         5,
					OrderID:        50,
					IdempotencyKey: "idem-key-prov-mis-555",
				})
				if err != nil {
					t.Fatalf("create payment: %v", err)
				}

				payload, _ := json.Marshal(service.WebhookPayload{
					EventID:           "evt_prov_mis_1",
					EventType:         "payment.succeeded",
					PaymentID:         payRes.Payment.ID,
					ProviderPaymentID: "ch_fraudulent_provider_id_different",
					AmountMinor:       50000,
					Currency:          "KZT",
				})
				now := time.Now().Unix()
				sig := signContractWebhook(secret, now, payload)

				err = svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now))
				if !errors.Is(err, domain.ErrProviderIDMismatch) {
					t.Fatalf("expected ErrProviderIDMismatch, got: %v", err)
				}

				secEvents, err := store.SecurityEvents().ListSecurityEvents(ctx)
				if err != nil {
					t.Fatalf("list security events: %v", err)
				}
				found := false
				for _, e := range secEvents {
					if e.EventType == "security.provider_id_mismatch" && e.PaymentID != nil && *e.PaymentID == payRes.Payment.ID {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected security.provider_id_mismatch event in DB, got: %+v", secEvents)
				}
			})
		})
	}
}

// 3. ProcessWebhook: Late Success requires refund and does NOT overwrite failed status
func TestContract_ProcessWebhook_LateSuccess_RequiresRefund(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()
			store, teardown := factory.setup(t)
			defer teardown()
			svc, _, _, secret := setupContractService(t, store)

			store.SeedUser(&domain.User{ID: 6, Email: "u6@kz", IsActive: true})
			store.SeedOrder(&domain.Order{ID: 60, UserID: 6, AmountMinor: 60000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

			payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
				UserID:         6,
				OrderID:        60,
				IdempotencyKey: "idem-key-late-succ-66",
			})
			if err != nil {
				t.Fatalf("create payment: %v", err)
			}

			// Платёж переведён в failed (например, отказ провайдера или таймаут сверки)
			if err := store.Payments().UpdateStatus(ctx, payRes.Payment.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
				t.Fatalf("update to failed: %v", err)
			}

			// Приходит запоздалый succeeded вебхук
			payload, _ := json.Marshal(service.WebhookPayload{
				EventID:           "evt_late_succ_1",
				EventType:         "payment.succeeded",
				PaymentID:         payRes.Payment.ID,
				ProviderPaymentID: payRes.Payment.ProviderPaymentID,
				AmountMinor:       60000,
				Currency:          "KZT",
			})
			now := time.Now().Unix()
			sig := signContractWebhook(secret, now, payload)

			// Вебхук возвращает nil (200 OK провайдеру), но фиксирует инцидент
			if err := svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now)); err != nil {
				t.Fatalf("process webhook: %v", err)
			}

			// Платёж НЕ должен быть перезаписан в succeeded!
			p, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
			if p.Status != domain.PaymentStatusFailed {
				t.Fatalf("payment status must remain failed, got: %s", p.Status)
			}

			// В payment_events должно быть зафиксировано событие payment.late_success_requires_refund
			events, err := store.PaymentEvents().ListByPaymentID(ctx, payRes.Payment.ID)
			if err != nil {
				t.Fatalf("list payment events: %v", err)
			}
			found := false
			for _, e := range events {
				if e.EventType == "payment.late_success_requires_refund" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected payment.late_success_requires_refund event in DB, got: %+v", events)
			}
		})
	}
}

// 4. ProcessWebhook: Atomicity Rollback on failure
func TestContract_ProcessWebhook_AtomicityRollback(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()
			store, teardown := factory.setup(t)
			defer teardown()
			svc, _, _, secret := setupContractService(t, store)

			store.SeedUser(&domain.User{ID: 7, Email: "u7@kz", IsActive: true})
			store.SeedOrder(&domain.Order{ID: 70, UserID: 7, AmountMinor: 70000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

			payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
				UserID:         7,
				OrderID:        70,
				IdempotencyKey: "idem-key-rollback-77",
			})
			if err != nil {
				t.Fatalf("create payment: %v", err)
			}

			// Посылаем некорректный webhook с пустым ProviderPaymentID (вызовет ошибку внутри транзакции)
			eventID := "evt_rollback_test_77"
			payload, _ := json.Marshal(service.WebhookPayload{
				EventID:           eventID,
				EventType:         "payment.succeeded",
				PaymentID:         payRes.Payment.ID,
				ProviderPaymentID: "", // ПУСТОЙ вызовет отказ
				AmountMinor:       70000,
				Currency:          "KZT",
			})
			now := time.Now().Unix()
			sig := signContractWebhook(secret, now, payload)

			err = svc.ProcessWebhook(ctx, payload, sig, fmt.Sprintf("%d", now))
			if err == nil {
				t.Fatal("expected error on missing ProviderPaymentID, got nil")
			}

			// Проверяем атомарность:
			// 1) Статус платежа остался pending
			p, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
			if p.Status != domain.PaymentStatusPending {
				t.Fatalf("expected payment pending, got: %s", p.Status)
			}

			// 2) Заказ остался unpaid
			o, _ := store.Orders().GetByID(ctx, 70)
			if o.Status != domain.OrderStatusUnpaid {
				t.Fatalf("expected order unpaid, got: %s", o.Status)
			}

			// 3) Запись в webhook_events ДОЛЖНА БЫТЬ ОТКАТАНА (не считать дубликатом при повторе с валидными данными)
			validPayload, _ := json.Marshal(service.WebhookPayload{
				EventID:           eventID, // ТОТ ЖЕ event_id
				EventType:         "payment.succeeded",
				PaymentID:         payRes.Payment.ID,
				ProviderPaymentID: payRes.Payment.ProviderPaymentID,
				AmountMinor:       70000,
				Currency:          "KZT",
			})
			validSig := signContractWebhook(secret, now, validPayload)

			// Повторная отправка с тем же event_id должна успешно обработаться (так как предыдущая попытка откатилась)
			if err := svc.ProcessWebhook(ctx, validPayload, validSig, fmt.Sprintf("%d", now)); err != nil {
				t.Fatalf("expected successful processing after rollback, got error: %v", err)
			}

			p2, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
			if p2.Status != domain.PaymentStatusSucceeded {
				t.Fatalf("expected payment succeeded after retry, got: %s", p2.Status)
			}
		})
	}
}

// 5. ProcessWebhook: 20 goroutines with the EXACT SAME event_id
func TestContract_ProcessWebhook_Concurrent20Goroutines(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()
			store, teardown := factory.setup(t)
			defer teardown()
			svc, _, _, secret := setupContractService(t, store)

			store.SeedUser(&domain.User{ID: 8, Email: "u8@kz", IsActive: true})
			store.SeedOrder(&domain.Order{ID: 80, UserID: 8, AmountMinor: 80000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

			payRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
				UserID:         8,
				OrderID:        80,
				IdempotencyKey: "idem-key-race-webhook-8",
			})
			if err != nil {
				t.Fatalf("create payment: %v", err)
			}

			eventID := fmt.Sprintf("evt_race_20_%d", time.Now().UnixNano())
			payload, _ := json.Marshal(service.WebhookPayload{
				EventID:           eventID,
				EventType:         "payment.succeeded",
				PaymentID:         payRes.Payment.ID,
				ProviderPaymentID: payRes.Payment.ProviderPaymentID,
				AmountMinor:       80000,
				Currency:          "KZT",
			})
			now := time.Now().Unix()
			sig := signContractWebhook(secret, now, payload)

			const goroutines = 20
			var wg sync.WaitGroup
			wg.Add(goroutines)
			errCh := make(chan error, goroutines)
			startGate := make(chan struct{})

			for i := 0; i < goroutines; i++ {
				go func() {
					defer wg.Done()
					<-startGate
					errCh <- svc.ProcessWebhook(context.Background(), payload, sig, fmt.Sprintf("%d", now))
				}()
			}

			close(startGate)
			wg.Wait()
			close(errCh)

			for err := range errCh {
				if err != nil {
					t.Fatalf("concurrent webhook processing returned error: %v", err)
				}
			}

			p, _ := store.Payments().GetByID(ctx, payRes.Payment.ID)
			if p.Status != domain.PaymentStatusSucceeded {
				t.Fatalf("expected payment succeeded, got %s", p.Status)
			}

			o, _ := store.Orders().GetByID(ctx, 80)
			if o.Status != domain.OrderStatusPaid {
				t.Fatalf("expected order paid, got %s", o.Status)
			}
		})
	}
}

// 6. CreatePayment: All domain error branches
func TestContract_CreatePayment_AllErrorBranches(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()
			store, teardown := factory.setup(t)
			defer teardown()
			svc, _, _, _ := setupContractService(t, store)

			// Seed users
			store.SeedUser(&domain.User{ID: 101, Email: "active@kz", IsActive: true, IsBlocked: false})
			store.SeedUser(&domain.User{ID: 102, Email: "inactive@kz", IsActive: false, IsBlocked: false})
			store.SeedUser(&domain.User{ID: 103, Email: "blocked@kz", IsActive: true, IsBlocked: true})

			// Seed orders
			store.SeedOrder(&domain.Order{ID: 201, UserID: 101, AmountMinor: 10000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
			store.SeedOrder(&domain.Order{ID: 202, UserID: 101, AmountMinor: 10000, Currency: "KZT", Status: domain.OrderStatusPaid})

			t.Run("inactive_user", func(t *testing.T) {
				store.SeedOrder(&domain.Order{ID: 205, UserID: 102, AmountMinor: 10000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
				_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         102,
					OrderID:        205,
					IdempotencyKey: "valid-idem-key-inactive-u",
				})
				if !errors.Is(err, domain.ErrUserInactive) {
					t.Fatalf("expected ErrUserInactive, got: %v", err)
				}
			})

			t.Run("blocked_user", func(t *testing.T) {
				store.SeedOrder(&domain.Order{ID: 206, UserID: 103, AmountMinor: 10000, Currency: "KZT", Status: domain.OrderStatusUnpaid})
				_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         103,
					OrderID:        206,
					IdempotencyKey: "valid-idem-key-blocked-u",
				})
				if !errors.Is(err, domain.ErrUserBlocked) {
					t.Fatalf("expected ErrUserBlocked, got: %v", err)
				}
			})

			t.Run("order_not_unpaid", func(t *testing.T) {
				_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
					UserID:         101,
					OrderID:        202, // Already paid
					IdempotencyKey: "valid-idem-key-not-unpaid",
				})
				if !errors.Is(err, domain.ErrOrderNotUnpaid) {
					t.Fatalf("expected ErrOrderNotUnpaid, got: %v", err)
				}
			})

			t.Run("order_invalid_amount", func(t *testing.T) {
				if factory.name == "sqlite" {
					// В SQLite инвариант 1 и целостность схемы защищены на уровне БД: CHECK (amount_minor > 0)
					sw := store.(*sqliteStoreWrapper)
					_, err := sw.DB().Exec(`
						INSERT INTO orders (id, user_id, amount_minor, currency, status)
						VALUES (203, 101, 0, 'KZT', 'unpaid')`)
					if err == nil {
						t.Fatal("expected SQLite CHECK constraint failure for amount_minor <= 0, got nil")
					}
				} else {
					store.SeedOrder(&domain.Order{ID: 203, UserID: 101, AmountMinor: 0, Currency: "KZT", Status: domain.OrderStatusUnpaid})
					_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
						UserID:         101,
						OrderID:        203,
						IdempotencyKey: "valid-idem-key-invalid-amt",
					})
					if !errors.Is(err, domain.ErrOrderInvalidAmount) {
						t.Fatalf("expected ErrOrderInvalidAmount, got: %v", err)
					}
				}
			})

			t.Run("unsupported_currency", func(t *testing.T) {
				if factory.name == "sqlite" {
					// В SQLite инвариант 1 и целостность схемы защищены на уровне БД: CHECK (currency = 'KZT')
					sw := store.(*sqliteStoreWrapper)
					_, err := sw.DB().Exec(`
						INSERT INTO orders (id, user_id, amount_minor, currency, status)
						VALUES (204, 101, 10000, 'USD', 'unpaid')`)
					if err == nil {
						t.Fatal("expected SQLite CHECK constraint failure for currency != 'KZT', got nil")
					}
				} else {
					store.SeedOrder(&domain.Order{ID: 204, UserID: 101, AmountMinor: 10000, Currency: "USD", Status: domain.OrderStatusUnpaid})
					_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
						UserID:         101,
						OrderID:        204, // USD
						IdempotencyKey: "valid-idem-key-unsupported-c",
					})
					if !errors.Is(err, domain.ErrUnsupportedCurrency) {
						t.Fatalf("expected ErrUnsupportedCurrency, got: %v", err)
					}
				}
			})

			t.Run("invalid_idempotency_key", func(t *testing.T) {
				invalidKeys := []string{"", "short", "bad characters!", "too-long-" + string(make([]byte, 200))}
				for _, k := range invalidKeys {
					_, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
						UserID:         101,
						OrderID:        201,
						IdempotencyKey: k,
					})
					if !errors.Is(err, domain.ErrInvalidIdempotencyKey) {
						t.Fatalf("expected ErrInvalidIdempotencyKey for key %q, got: %v", k, err)
					}
				}
			})
		})
	}
}

// 7. Reconciler: Provider Succeeded marks payment Succeeded and order Paid on memory and sqlite
func TestContract_Reconciler_ProviderSucceeded_MarksOrderPaid(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()
			store, teardown := factory.setup(t)
			defer teardown()

			prov := &fakeProvider{
				statusByID: map[string]domain.PaymentStatus{
					"ch_reconcile_succ_1": domain.PaymentStatusSucceeded,
				},
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))

			store.SeedUser(&domain.User{ID: 301, Email: "rec@kz", IsActive: true})
			store.SeedOrder(&domain.Order{ID: 301, UserID: 301, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusUnpaid})

			oldTime := time.Now().UTC().Add(-30 * time.Minute)
			p := &domain.Payment{
				UserID:            301,
				OrderID:           301,
				AmountMinor:       50000,
				Currency:          "KZT",
				Status:            domain.PaymentStatusPending,
				IdempotencyKey:    "contract-reconcile-key-1",
				RequestHash:       "hash-rec",
				ProviderPaymentID: "ch_reconcile_succ_1",
				CreatedAt:         oldTime,
				UpdatedAt:         oldTime,
			}
			if err := store.Payments().CreatePending(ctx, p); err != nil {
				t.Fatalf("create pending: %v", err)
			}
			if err := store.Payments().UpdateSession(ctx, p.ID, "ch_reconcile_succ_1", "https://checkout.fake/pay"); err != nil {
				t.Fatalf("update session: %v", err)
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
					Interval: 1 * time.Minute,
					Batch:    10,
				},
			)

			count, err := reconciler.ReconcileOnce(ctx)
			if err != nil {
				t.Fatalf("reconcile once failed: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected 1 reconciled payment, got %d", count)
			}

			pUpdated, err := store.Payments().GetByID(ctx, p.ID)
			if err != nil || pUpdated.Status != domain.PaymentStatusSucceeded {
				t.Fatalf("expected payment succeeded, got %v (err=%v)", pUpdated.Status, err)
			}

			oUpdated, err := store.Orders().GetByID(ctx, 301)
			if err != nil || oUpdated.Status != domain.OrderStatusPaid {
				t.Fatalf("expected order paid, got %v (err=%v)", oUpdated.Status, err)
			}
		})
	}
}

// 8. Reconciler: When order is already Paid, Reconciler handles duplicate succeeded payment gracefully without infinite loop
func TestContract_Reconciler_OrderAlreadyPaid_HandlesDuplicateGracefully(t *testing.T) {
	for _, factory := range getStoreFactories(t) {
		t.Run(factory.name, func(t *testing.T) {
			ctx := context.Background()
			store, teardown := factory.setup(t)
			defer teardown()

			prov := &fakeProvider{
				statusByID: map[string]domain.PaymentStatus{
					"ch_dup_succ_1": domain.PaymentStatusSucceeded,
				},
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))

			store.SeedUser(&domain.User{ID: 401, Email: "rec_dup@kz", IsActive: true})
			// Заказ уже оплачен!
			store.SeedOrder(&domain.Order{ID: 401, UserID: 401, AmountMinor: 50000, Currency: "KZT", Status: domain.OrderStatusPaid})

			oldTime := time.Now().UTC().Add(-30 * time.Minute)
			p := &domain.Payment{
				UserID:            401,
				OrderID:           401,
				AmountMinor:       50000,
				Currency:          "KZT",
				Status:            domain.PaymentStatusPending,
				IdempotencyKey:    "contract-reconcile-dup-key-1",
				RequestHash:       "hash-rec-dup",
				ProviderPaymentID: "ch_dup_succ_1",
				CreatedAt:         oldTime,
				UpdatedAt:         oldTime,
			}
			if err := store.Payments().CreatePending(ctx, p); err != nil {
				t.Fatalf("create pending: %v", err)
			}
			if err := store.Payments().UpdateSession(ctx, p.ID, "ch_dup_succ_1", "https://checkout.fake/pay"); err != nil {
				t.Fatalf("update session: %v", err)
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
					Interval: 1 * time.Minute,
					Batch:    10,
				},
			)

			count, err := reconciler.ReconcileOnce(ctx)
			if err != nil {
				t.Fatalf("reconcile once failed: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected 1 reconciled payment, got %d", count)
			}

			pUpdated, err := store.Payments().GetByID(ctx, p.ID)
			if err != nil {
				t.Fatalf("get payment: %v", err)
			}
			if pUpdated.Status == domain.PaymentStatusPending {
				t.Fatalf("payment must not remain pending after reconciliation")
			}

			// Проверяем, что зафиксировано событие о дубликате, требующем возврата
			events, err := store.PaymentEvents().ListByPaymentID(ctx, p.ID)
			if err != nil {
				t.Fatalf("list payment events: %v", err)
			}
			foundRefundAlert := false
			for _, ev := range events {
				if ev.EventType == "reconciliation.duplicate_requires_refund" {
					foundRefundAlert = true
					break
				}
			}
			if !foundRefundAlert {
				t.Fatalf("expected reconciliation.duplicate_requires_refund event, got %+v", events)
			}
		})
	}
}
