package service

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
	"regexp"
	"strconv"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/metrics"
	"github.com/Senjumarru/miniwallet/internal/provider"
)

var idempotencyKeyRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`)

type CreatePaymentInput struct {
	UserID         int64
	OrderID        int64
	IdempotencyKey string
}

type CreatePaymentResult struct {
	Payment     *domain.Payment
	CheckoutURL string
	IsReplay    bool
}

type PaymentService struct {
	users          UserRepository
	orders         OrderRepository
	payments       PaymentRepository
	webhooks       WebhookEventRepository
	paymentEvents  PaymentEventRepository
	securityEvents SecurityEventRepository
	txManager      TxManager
	provider       provider.PaymentProvider
	logger         *slog.Logger
	clock          domain.Clock
	metrics        *metrics.Metrics
	webhookSecrets []string
}

func NewPaymentService(
	users UserRepository,
	orders OrderRepository,
	payments PaymentRepository,
	webhooks WebhookEventRepository,
	paymentEvents PaymentEventRepository,
	securityEvents SecurityEventRepository,
	txManager TxManager,
	provider provider.PaymentProvider,
	logger *slog.Logger,
	webhookSecrets ...string,
) *PaymentService {
	return &PaymentService{
		users:          users,
		orders:         orders,
		payments:       payments,
		webhooks:       webhooks,
		paymentEvents:  paymentEvents,
		securityEvents: securityEvents,
		txManager:      txManager,
		provider:       provider,
		logger:         logger,
		clock:          domain.RealClock{},
		webhookSecrets: webhookSecrets,
	}
}

func (s *PaymentService) SetClock(clock domain.Clock) {
	s.clock = clock
}

func (s *PaymentService) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
}

func (s *PaymentService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *PaymentService) CreatePayment(ctx context.Context, in CreatePaymentInput) (CreatePaymentResult, error) {
	// 1. Валидация формата Idempotency-Key
	if !idempotencyKeyRegex.MatchString(in.IdempotencyKey) {
		return CreatePaymentResult{}, domain.ErrInvalidIdempotencyKey
	}

	requestHash := computeRequestHash(in.UserID, in.OrderID)

	// 2. БЫСТРОЕ ЧТЕНИЕ по (user_id, idempotency_key) ДО проверок заказа (п. 2.3)
	existing, err := s.payments.GetByIdempotencyKey(ctx, in.UserID, in.IdempotencyKey)
	if err != nil && !errors.Is(err, domain.ErrPaymentNotFound) {
		// Ошибка чтения по ключу кроме ErrPaymentNotFound — внутренняя (п. 2.4)
		return CreatePaymentResult{}, fmt.Errorf("read payment by idempotency key: %w", err)
	}

	if existing != nil {
		// 1) Конфликт параметров запроса (другой order/amount/request hash)
		if existing.RequestHash != requestHash {
			s.logger.WarnContext(ctx, "idempotency conflict detected",
				slog.Int64("user_id", in.UserID),
				slog.Int64("order_id", in.OrderID),
				slog.String("idempotency_key", in.IdempotencyKey),
			)
			return CreatePaymentResult{}, domain.ErrIdempotencyConflict
		}

		// 2) Succeeded -> повтор со статусом (200 OK)
		if existing.Status == domain.PaymentStatusSucceeded {
			s.logger.InfoContext(ctx, "idempotent payment replay: succeeded",
				slog.Int64("payment_id", existing.ID),
				slog.Int64("order_id", existing.OrderID),
			)
			return CreatePaymentResult{
				Payment:     existing,
				CheckoutURL: existing.CheckoutURL,
				IsReplay:    true,
			}, nil
		}

		// 3) Failed / Canceled -> явная ошибка ErrPaymentNotPayable (409, "используйте новый Idempotency-Key"), НЕ 200 (п. 2.3)
		if existing.Status == domain.PaymentStatusFailed || existing.Status == domain.PaymentStatusCanceled {
			return CreatePaymentResult{}, domain.ErrPaymentNotPayable
		}

		// 4) Pending с CheckoutURL -> повтор (200 OK)
		if existing.CheckoutURL != "" {
			s.logger.InfoContext(ctx, "idempotent payment replay: pending session attached",
				slog.Int64("payment_id", existing.ID),
				slog.Int64("order_id", existing.OrderID),
			)
			return CreatePaymentResult{
				Payment:     existing,
				CheckoutURL: existing.CheckoutURL,
				IsReplay:    true,
			}, nil
		}

		// 5) Pending без CheckoutURL -> первый запрос в процессе создания сессии у провайдера -> 409 с Retry-After (п. 2.3)
		return CreatePaymentResult{}, domain.ErrRequestInProgress
	}

	// 3. Проверка пользователя (только для новых платежей)
	user, err := s.users.GetByID(ctx, in.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			return CreatePaymentResult{}, domain.ErrUserNotFound
		}
		return CreatePaymentResult{}, fmt.Errorf("get user: %w", err)
	}
	if !user.IsActive {
		return CreatePaymentResult{}, domain.ErrUserInactive
	}
	if user.IsBlocked {
		return CreatePaymentResult{}, domain.ErrUserBlocked
	}

	// 4. Проверка заказа (сумму и валюту берём исключительно из БД)
	order, err := s.orders.GetByID(ctx, in.OrderID)
	if err != nil {
		if errors.Is(err, domain.ErrOrderNotFound) {
			return CreatePaymentResult{}, domain.ErrOrderNotFound
		}
		return CreatePaymentResult{}, fmt.Errorf("get order: %w", err)
	}
	// Чужой и несуществующий заказ возвращают одну ошибку ErrOrderNotFound (п. 2.5)
	if order.UserID != in.UserID {
		return CreatePaymentResult{}, domain.ErrOrderNotFound
	}
	if order.Status != domain.OrderStatusUnpaid {
		return CreatePaymentResult{}, domain.ErrOrderNotUnpaid
	}
	if order.AmountMinor <= 0 {
		return CreatePaymentResult{}, domain.ErrOrderInvalidAmount
	}
	if order.Currency != domain.CurrencyKZT {
		return CreatePaymentResult{}, domain.ErrUnsupportedCurrency
	}

	// 5. АТОМАРНАЯ ВСТАВКА PENDING-ПЛАТЕЖА (арбитр гонки, п. 2.4)
	payment := &domain.Payment{
		UserID:         in.UserID,
		OrderID:        in.OrderID,
		AmountMinor:    order.AmountMinor,
		Currency:       order.Currency,
		Status:         domain.PaymentStatusPending,
		IdempotencyKey: in.IdempotencyKey,
		RequestHash:    requestHash,
	}

	if err := s.payments.CreatePending(ctx, payment); err != nil {
		var uniqErr *domain.ErrUniqueViolation
		if errors.As(err, &uniqErr) {
			// Нарушение уникальности: перечитываем по ключу
			existingByKey, getErr := s.payments.GetByIdempotencyKey(ctx, in.UserID, in.IdempotencyKey)
			if getErr != nil && !errors.Is(getErr, domain.ErrPaymentNotFound) {
				// Ошибка чтения по ключу кроме ErrPaymentNotFound — внутренняя (п. 2.4)
				return CreatePaymentResult{}, fmt.Errorf("re-read payment after unique collision: %w", getErr)
			}

			if existingByKey != nil {
				// Ключ найден -> решаем повтор / конфликт
				if existingByKey.RequestHash != requestHash {
					s.logger.WarnContext(ctx, "idempotency conflict detected",
						slog.Int64("user_id", in.UserID),
						slog.Int64("order_id", in.OrderID),
						slog.String("idempotency_key", in.IdempotencyKey),
					)
					return CreatePaymentResult{}, domain.ErrIdempotencyConflict
				}
				if existingByKey.Status == domain.PaymentStatusSucceeded {
					return CreatePaymentResult{
						Payment:     existingByKey,
						CheckoutURL: existingByKey.CheckoutURL,
						IsReplay:    true,
					}, nil
				}
				if existingByKey.Status == domain.PaymentStatusFailed || existingByKey.Status == domain.PaymentStatusCanceled {
					return CreatePaymentResult{}, domain.ErrPaymentNotPayable
				}
				if existingByKey.CheckoutURL != "" {
					return CreatePaymentResult{
						Payment:     existingByKey,
						CheckoutURL: existingByKey.CheckoutURL,
						IsReplay:    true,
					}, nil
				}
				return CreatePaymentResult{}, domain.ErrRequestInProgress
			}

			// Ключ не найден: ограничение на один активный pending-платёж на заказ (другой ключ)
			return CreatePaymentResult{}, domain.ErrActivePaymentExists
		}
		// Прочие ошибки оборачиваем через %w и возвращаем как внутренние (НЕ ErrActivePaymentExists, п. 2.4)
		return CreatePaymentResult{}, fmt.Errorf("create pending payment: %w", err)
	}

	if recErr := s.paymentEvents.RecordEvent(ctx, domain.PaymentEvent{
		PaymentID: payment.ID,
		EventType: "payment.created",
		ToStatus:  string(domain.PaymentStatusPending),
		Metadata:  fmt.Sprintf(`{"order_id":%d,"amount_minor":%d}`, order.ID, order.AmountMinor),
	}); recErr != nil {
		s.logger.ErrorContext(ctx, "failed to record payment.created event",
			slog.Int64("payment_id", payment.ID),
			slog.String("error", recErr.Error()),
		)
	}

	s.logger.InfoContext(ctx, "payment created in db",
		slog.Int64("payment_id", payment.ID),
		slog.Int64("order_id", payment.OrderID),
		slog.Int64("amount_minor", payment.AmountMinor),
		slog.String("currency", payment.Currency),
	)

	// 6. Вызов платёжного провайдера (Ключ для провайдера = pay-%d, п. 2.1)
	providerKey := fmt.Sprintf("pay-%d", payment.ID)
	session, err := s.provider.CreateCheckoutSession(ctx, provider.CreateCheckoutInput{
		PaymentID:      payment.ID,
		OrderID:        payment.OrderID,
		AmountMinor:    payment.AmountMinor,
		Currency:       payment.Currency,
		IdempotencyKey: providerKey,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "provider checkout session creation failed",
			slog.Int64("payment_id", payment.ID),
			slog.Int64("order_id", payment.OrderID),
			slog.String("error", err.Error()),
		)

		// Отдельный контекст с таймаутом для надёжной фиксации статуса (п. 2.2)
		detachedCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if provider.IsDefinitiveRejection(err) {
			// Однозначный отказ (4xx кроме 429) -> переводим в failed, освобождаем заказ (п. 2.2)
			if upErr := s.payments.UpdateStatus(detachedCtx, payment.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); upErr != nil {
				s.logger.ErrorContext(detachedCtx, "failed to update payment status to failed",
					slog.Int64("payment_id", payment.ID),
					slog.String("error", upErr.Error()),
				)
			}
			if evErr := s.paymentEvents.RecordEvent(detachedCtx, domain.PaymentEvent{
				PaymentID:  payment.ID,
				EventType:  "payment.provider_rejected",
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(domain.PaymentStatusFailed),
				Metadata:   fmt.Sprintf(`{"error":%q}`, err.Error()),
			}); evErr != nil {
				s.logger.ErrorContext(detachedCtx, "failed to record payment.provider_rejected event",
					slog.Int64("payment_id", payment.ID),
					slog.String("error", evErr.Error()),
				)
			}
		} else {
			// Неоднозначный исход (таймаут, 5xx, разрыв) -> платёж остаётся pending, заказ НЕ освобождается (п. 2.2)
			s.logger.WarnContext(detachedCtx, "provider call had ambiguous outcome; payment remains pending for reconciler",
				slog.Int64("payment_id", payment.ID),
				slog.String("error", err.Error()),
			)
			if evErr := s.paymentEvents.RecordEvent(detachedCtx, domain.PaymentEvent{
				PaymentID: payment.ID,
				EventType: "payment.provider_ambiguous_failure",
				Metadata:  fmt.Sprintf(`{"error":%q}`, err.Error()),
			}); evErr != nil {
				s.logger.ErrorContext(detachedCtx, "failed to record payment.provider_ambiguous_failure event",
					slog.Int64("payment_id", payment.ID),
					slog.String("error", evErr.Error()),
				)
			}
		}

		return CreatePaymentResult{}, fmt.Errorf("provider checkout: %w", err)
	}

	// 7. Обновление сессии платежа
	if err := s.payments.UpdateSession(ctx, payment.ID, session.ProviderPaymentID, session.CheckoutURL); err != nil {
		// Сбой UpdateSession после успеха у провайдера: платёж остаётся pending, лог с payment_id и provider_payment_id, дальше решает сверка (п. 2.6)
		s.logger.ErrorContext(ctx, "failed to update payment session after successful provider session; payment remains pending for reconciliation",
			slog.Int64("payment_id", payment.ID),
			slog.String("provider_payment_id", session.ProviderPaymentID),
			slog.String("error", err.Error()),
		)
		return CreatePaymentResult{}, fmt.Errorf("update payment session: %w", err)
	}

	if evErr := s.paymentEvents.RecordEvent(ctx, domain.PaymentEvent{
		PaymentID: payment.ID,
		EventType: "payment.session_attached",
		Metadata:  fmt.Sprintf(`{"provider_payment_id":%q}`, session.ProviderPaymentID),
	}); evErr != nil {
		s.logger.ErrorContext(ctx, "failed to record payment.session_attached event",
			slog.Int64("payment_id", payment.ID),
			slog.String("error", evErr.Error()),
		)
	}

	payment.ProviderPaymentID = session.ProviderPaymentID
	payment.CheckoutURL = session.CheckoutURL

	return CreatePaymentResult{
		Payment:     payment,
		CheckoutURL: session.CheckoutURL,
		IsReplay:    false,
	}, nil
}

// TODO(docs): Структура входящего вебхука от провайдера.
type WebhookPayload struct {
	EventID           string `json:"event_id"`
	EventType         string `json:"event_type"`
	PaymentID         int64  `json:"payment_id"`
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
	AmountMinor       int64  `json:"amount_minor"`
	Currency          string `json:"currency"`
}

// VerifyWebhookSignature проверяет подпись вебхука и временную метку (±5 минут) против replay-атак.
func (s *PaymentService) VerifyWebhookSignature(rawBody []byte, signature, timestampStr string) error {
	// 1. Проверка временной метки события (±5 минут) против replay-атак через Clock
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return domain.ErrExpiredTimestamp
	}

	eventTime := time.Unix(timestamp, 0).UTC()
	now := s.now()
	diff := now.Sub(eventTime)
	if diff < -5*time.Minute || diff > 5*time.Minute {
		return domain.ErrExpiredTimestamp
	}

	// 2. Проверка подписи по сырому телу через constant-time сравнение hmac.Equal
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return domain.ErrInvalidSignature
	}

	sigPayload := []byte(fmt.Sprintf("%d.", timestamp))
	sigPayload = append(sigPayload, rawBody...)

	// 3.8 Два активных секрета вебхука одновременно (ротация без простоя)
	for _, secret := range s.webhookSecrets {
		if secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(sigPayload)
		expectedMAC := mac.Sum(nil)
		if hmac.Equal(sigBytes, expectedMAC) {
			return nil
		}
	}

	return domain.ErrInvalidSignature
}

func (s *PaymentService) ProcessWebhook(ctx context.Context, rawBody []byte, signature, timestampStr string) error {
	if err := s.VerifyWebhookSignature(rawBody, signature, timestampStr); err != nil {
		s.logger.WarnContext(ctx, "security event: webhook signature verification failed",
			slog.String("error", err.Error()),
		)
		return err
	}

	// 3. Парсинг валидированного JSON
	var payload WebhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		s.logger.WarnContext(ctx, "failed to unmarshal validly signed webhook json", slog.String("error", err.Error()))
		return fmt.Errorf("unmarshal webhook body: %w", err)
	}

	if payload.EventID == "" {
		return fmt.Errorf("missing event_id in webhook payload")
	}

	var postCommitErr error

	// 4. Выполнение в транзакции
	txErr := s.txManager.WithinTransaction(ctx, func(txCtx context.Context) error {
		// Дедупликация: повторная доставка одного event_id возвращает 200 без повторной обработки
		isDuplicate, err := s.webhooks.RecordEvent(txCtx, payload.EventID, payload.EventType, rawBody)
		if err != nil {
			return fmt.Errorf("record webhook event: %w", err)
		}
		if isDuplicate {
			s.logger.InfoContext(txCtx, "duplicate webhook event ignored",
				slog.String("event_id", payload.EventID),
				slog.String("event_type", payload.EventType),
			)
			return nil
		}

		s.logger.InfoContext(txCtx, "webhook received",
			slog.String("event_id", payload.EventID),
			slog.String("event_type", payload.EventType),
			slog.Int64("payment_id", payload.PaymentID),
		)

		// Неизвестный тип события — логируем и завершаем успешно (HTTP 200)
		switch payload.EventType {
		case "payment.succeeded", "payment.failed", "payment.canceled":
		default:
			s.logger.InfoContext(txCtx, "ignoring unknown webhook event type",
				slog.String("event_type", payload.EventType),
			)
			return nil
		}

		// Загрузка платежа строго через транзакцию
		payment, err := s.payments.GetByID(txCtx, payload.PaymentID)
		if err != nil {
			if errors.Is(err, domain.ErrPaymentNotFound) {
				// 3.5 Неизвестный payment_id: security_events, метрика, ответ 200 (повторять нечего)
				s.logger.WarnContext(txCtx, "security event: webhook for unknown payment_id",
					slog.Int64("payment_id", payload.PaymentID),
					slog.String("event_id", payload.EventID),
				)
				if s.securityEvents != nil {
					if secErr := s.securityEvents.RecordSecurityEvent(txCtx, domain.SecurityEvent{
						EventType: "security.unknown_payment_id",
						Metadata:  fmt.Sprintf(`{"payment_id":%d,"event_id":%q}`, payload.PaymentID, payload.EventID),
					}); secErr != nil {
						return fmt.Errorf("record security event for unknown payment: %w", secErr)
					}
				}
				return nil
			}
			return fmt.Errorf("load payment %d: %w", payload.PaymentID, err)
		}

		// 3.4 payment.succeeded требует непустой ProviderPaymentID в вебхуке; если у платежа он пуст — привяжи и запиши событие. Если пуст в вебхуке — отказ.
		if payload.EventType == "payment.succeeded" {
			if payload.ProviderPaymentID == "" {
				s.logger.WarnContext(txCtx, "security event: payment.succeeded webhook missing provider_payment_id",
					slog.Int64("payment_id", payment.ID),
					slog.String("event_id", payload.EventID),
				)
				if s.securityEvents != nil {
					if secErr := s.securityEvents.RecordSecurityEvent(txCtx, domain.SecurityEvent{
						PaymentID: &payment.ID,
						EventType: "security.missing_provider_payment_id",
						Metadata:  fmt.Sprintf(`{"event_id":%q}`, payload.EventID),
					}); secErr != nil {
						return fmt.Errorf("record security event: %w", secErr)
					}
				}
				return fmt.Errorf("missing provider_payment_id in payment.succeeded webhook")
			}
			if payment.ProviderPaymentID == "" {
				if err := s.payments.UpdateSession(txCtx, payment.ID, payload.ProviderPaymentID, payment.CheckoutURL); err != nil {
					return fmt.Errorf("attach provider_payment_id: %w", err)
				}
				payment.ProviderPaymentID = payload.ProviderPaymentID
				if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
					PaymentID: payment.ID,
					EventType: "payment.provider_id_attached",
					Metadata:  fmt.Sprintf(`{"provider_payment_id":%q}`, payload.ProviderPaymentID),
				}); recErr != nil {
					return fmt.Errorf("record provider_id_attached event: %w", recErr)
				}
			}
		}

		// 3.1 & 3.4 Сверка ProviderPaymentID (если у платежа уже есть ProviderPaymentID)
		if payload.ProviderPaymentID != "" && payment.ProviderPaymentID != "" && payload.ProviderPaymentID != payment.ProviderPaymentID {
			s.logger.ErrorContext(txCtx, "security event: provider payment id mismatch",
				slog.Int64("payment_id", payment.ID),
				slog.String("expected_provider_id", payment.ProviderPaymentID),
				slog.String("webhook_provider_id", payload.ProviderPaymentID),
			)
			if s.securityEvents != nil {
				if secErr := s.securityEvents.RecordSecurityEvent(txCtx, domain.SecurityEvent{
					PaymentID: &payment.ID,
					EventType: "security.provider_id_mismatch",
					Metadata:  fmt.Sprintf(`{"expected":%q,"got":%q}`, payment.ProviderPaymentID, payload.ProviderPaymentID),
				}); secErr != nil {
					return fmt.Errorf("record security event: %w", secErr)
				}
			}
			if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
				PaymentID: payment.ID,
				EventType: "security.provider_id_mismatch",
				Metadata:  fmt.Sprintf(`{"expected":%q,"got":%q}`, payment.ProviderPaymentID, payload.ProviderPaymentID),
			}); recErr != nil {
				return fmt.Errorf("record provider_id_mismatch event: %w", recErr)
			}
			if s.metrics != nil {
				s.metrics.WebhookMismatchTotal.WithLabelValues("provider_id").Inc()
			}
			postCommitErr = domain.ErrProviderIDMismatch
			return nil // КОММИТИТСЯ, ошибка возвращается после коммита (п. 3.1)
		}

		// 3.2 Сверка суммы и валюты с payment.AmountMinor/Currency, а не с заказом
		if payload.AmountMinor != payment.AmountMinor || payload.Currency != payment.Currency {
			s.logger.ErrorContext(txCtx, "security event: webhook amount or currency mismatch with payment",
				slog.Int64("payment_id", payment.ID),
				slog.Int64("webhook_amount", payload.AmountMinor),
				slog.Int64("payment_amount", payment.AmountMinor),
				slog.String("webhook_currency", payload.Currency),
				slog.String("payment_currency", payment.Currency),
			)
			if s.securityEvents != nil {
				if secErr := s.securityEvents.RecordSecurityEvent(txCtx, domain.SecurityEvent{
					PaymentID: &payment.ID,
					EventType: "security.amount_mismatch",
					Metadata:  fmt.Sprintf(`{"payment_amount":%d,"webhook_amount":%d,"payment_currency":%q,"webhook_currency":%q}`, payment.AmountMinor, payload.AmountMinor, payment.Currency, payload.Currency),
				}); secErr != nil {
					return fmt.Errorf("record security event: %w", secErr)
				}
			}
			if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
				PaymentID: payment.ID,
				EventType: "security.amount_mismatch",
				Metadata:  fmt.Sprintf(`{"payment_amount":%d,"webhook_amount":%d}`, payment.AmountMinor, payload.AmountMinor),
			}); recErr != nil {
				return fmt.Errorf("record amount_mismatch event: %w", recErr)
			}
			if s.metrics != nil {
				s.metrics.WebhookMismatchTotal.WithLabelValues("amount").Inc()
			}
			postCommitErr = domain.ErrWebhookAmountMismatch
			return nil // КОММИТИТСЯ, ошибка возвращается после коммита (п. 3.1)
		}

		// Загрузка заказа строго через транзакцию
		order, err := s.orders.GetByID(txCtx, payment.OrderID)
		if err != nil {
			return fmt.Errorf("load order %d: %w", payment.OrderID, err)
		}

		// 6. Обработка смены статусов
		switch payload.EventType {
		case "payment.succeeded":
			// 3.3 Если order уже paid: платёж -> succeeded, событие payment.duplicate_requires_refund, коммит, без бесконечных повторов;
			// нарушение уникального индекса succeeded обрабатывается явно.
			if order.Status == domain.OrderStatusPaid {
				s.logger.WarnContext(txCtx, "order already paid; duplicate payment requires refund",
					slog.Int64("payment_id", payment.ID),
					slog.Int64("order_id", order.ID),
				)
				upErr := s.payments.UpdateStatus(txCtx, payment.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded)
				if upErr != nil {
					var uniqErr *domain.ErrUniqueViolation
					if errors.As(upErr, &uniqErr) {
						s.logger.WarnContext(txCtx, "duplicate succeeded payment unique index conflict handled",
							slog.Int64("payment_id", payment.ID),
							slog.Int64("order_id", order.ID),
						)
					} else if !errors.Is(upErr, domain.ErrStatusConflict) {
						return fmt.Errorf("update duplicate payment status: %w", upErr)
					}
				}
				if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
					PaymentID:  payment.ID,
					EventType:  "payment.duplicate_requires_refund",
					FromStatus: string(payment.Status),
					ToStatus:   string(domain.PaymentStatusSucceeded),
					Metadata:   fmt.Sprintf(`{"order_id":%d,"alert":"duplicate_payment_for_paid_order"}`, order.ID),
				}); recErr != nil {
					return fmt.Errorf("record duplicate_requires_refund event: %w", recErr)
				}
				return nil
			}

			// 3.7 Late success (успех по failed/canceled): событие payment.late_success_requires_refund, метрика, строка в списке ручного разбора
			if !payment.Status.CanTransitionTo(domain.PaymentStatusSucceeded) {
				s.logger.WarnContext(txCtx, "ignoring status transition on finalized payment",
					slog.Int64("payment_id", payment.ID),
					slog.String("current_status", string(payment.Status)),
					slog.String("target_status", string(domain.PaymentStatusSucceeded)),
				)
				if payment.Status == domain.PaymentStatusFailed || payment.Status == domain.PaymentStatusCanceled {
					if s.metrics != nil {
						s.metrics.WebhookLateSuccessTotal.Inc()
					}
					if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
						PaymentID:  payment.ID,
						EventType:  "payment.late_success_requires_refund",
						FromStatus: string(payment.Status),
						ToStatus:   string(domain.PaymentStatusSucceeded),
						Metadata:   `{"alert":"user_charged_after_finalized_status"}`,
					}); recErr != nil {
						return fmt.Errorf("record late_success event: %w", recErr)
					}
				}
				return nil
			}

			if err := s.payments.UpdateStatus(txCtx, payment.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
				return fmt.Errorf("update payment status to succeeded: %w", err)
			}
			if err := s.orders.UpdateStatus(txCtx, order.ID, domain.OrderStatusUnpaid, domain.OrderStatusPaid); err != nil {
				return fmt.Errorf("update order status to paid: %w", err)
			}
			if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
				PaymentID:  payment.ID,
				EventType:  "payment.succeeded",
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(domain.PaymentStatusSucceeded),
				Metadata:   fmt.Sprintf(`{"event_id":%q}`, payload.EventID),
			}); recErr != nil {
				return fmt.Errorf("record payment.succeeded event: %w", recErr)
			}
			if s.metrics != nil {
				s.metrics.PaymentsTotal.WithLabelValues("succeeded").Inc()
				s.metrics.PaymentsPendingCount.Dec()
			}

		case "payment.failed":
			if !payment.Status.CanTransitionTo(domain.PaymentStatusFailed) {
				return nil
			}
			if err := s.payments.UpdateStatus(txCtx, payment.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
				return fmt.Errorf("update payment status to failed: %w", err)
			}
			if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
				PaymentID:  payment.ID,
				EventType:  "payment.failed",
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(domain.PaymentStatusFailed),
				Metadata:   fmt.Sprintf(`{"event_id":%q}`, payload.EventID),
			}); recErr != nil {
				return fmt.Errorf("record payment.failed event: %w", recErr)
			}
			if s.metrics != nil {
				s.metrics.PaymentsTotal.WithLabelValues("failed").Inc()
				s.metrics.PaymentsPendingCount.Dec()
			}

		case "payment.canceled":
			if !payment.Status.CanTransitionTo(domain.PaymentStatusCanceled) {
				return nil
			}
			if err := s.payments.UpdateStatus(txCtx, payment.ID, domain.PaymentStatusPending, domain.PaymentStatusCanceled); err != nil {
				return fmt.Errorf("update payment status to canceled: %w", err)
			}
			if recErr := s.paymentEvents.RecordEvent(txCtx, domain.PaymentEvent{
				PaymentID:  payment.ID,
				EventType:  "payment.canceled",
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(domain.PaymentStatusCanceled),
				Metadata:   fmt.Sprintf(`{"event_id":%q}`, payload.EventID),
			}); recErr != nil {
				return fmt.Errorf("record payment.canceled event: %w", recErr)
			}
		}

		return nil
	})

	if txErr != nil {
		return txErr
	}
	if postCommitErr != nil {
		return postCommitErr
	}
	return nil
}

func computeRequestHash(userID, orderID int64) string {
	h := sha256.New()
	if _, err := io.WriteString(h, fmt.Sprintf("%d:%d", userID, orderID)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
