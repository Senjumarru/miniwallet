package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	paymentSvc   *service.PaymentService
	logger       *slog.Logger
	jwtMgr       *JWTManager
	readyChecker func(context.Context) error
}

type HandlerOptions struct {
	PaymentsUserLimiter *RateLimiter
	PaymentsIPLimiter   *RateLimiter
	WebhookLimiter      *RateLimiter
	ReadyChecker        func(context.Context) error
}

func WithPaymentsUserLimiter(l *RateLimiter) func(*HandlerOptions) {
	return func(o *HandlerOptions) {
		o.PaymentsUserLimiter = l
	}
}

func WithPaymentsIPLimiter(l *RateLimiter) func(*HandlerOptions) {
	return func(o *HandlerOptions) {
		o.PaymentsIPLimiter = l
	}
}

func WithWebhookLimiter(l *RateLimiter) func(*HandlerOptions) {
	return func(o *HandlerOptions) {
		o.WebhookLimiter = l
	}
}

func WithReadyChecker(fn func(context.Context) error) func(*HandlerOptions) {
	return func(o *HandlerOptions) {
		o.ReadyChecker = fn
	}
}

func NewHandler(paymentSvc *service.PaymentService, logger *slog.Logger, jwtSecret string, opts ...func(*HandlerOptions)) http.Handler {
	options := HandlerOptions{
		PaymentsUserLimiter: NewRateLimiter(10, 20, domain.RealClock{}),
		PaymentsIPLimiter:   NewRateLimiter(20, 40, domain.RealClock{}),
		WebhookLimiter:      NewRateLimiter(50, 100, domain.RealClock{}),
	}
	for _, opt := range opts {
		opt(&options)
	}

	jwtMgr := NewJWTManager([]byte(jwtSecret))
	s := &Server{
		paymentSvc:   paymentSvc,
		logger:       logger,
		jwtMgr:       jwtMgr,
		readyChecker: options.ReadyChecker,
	}

	mux := http.NewServeMux()

	// 5.1 & 5.4: POST /payments защищён IP rate limit -> JWT middleware -> User rate limit
	paymentsHandler := IPRateLimitMiddleware(options.PaymentsIPLimiter)(
		JWTMiddleware(jwtMgr)(
			UserRateLimitMiddleware(options.PaymentsUserLimiter)(
				http.HandlerFunc(s.handleCreatePayment),
			),
		),
	)
	mux.Handle("POST /payments", paymentsHandler)

	// 5.4: POST /webhooks/provider защищён отдельным webhook rate limit
	webhookHandler := WebhookRateLimitMiddleware(options.WebhookLimiter)(
		http.HandlerFunc(s.handleWebhook),
	)
	mux.Handle("POST /webhooks/provider", webhookHandler)

	// 6.4: Prometheus метрики и эндпоинты проверки жизнеспособности и готовности
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.Handler())

	return RequestIDMiddleware(LoggingMiddleware(logger)(mux))
}

type createPaymentRequest struct {
	// 5.1: Заголовок X-User-ID и user_id в теле игнорируются.
	// Идентификатор клиента берется ТОЛЬКО из проверенного JWT токена.
	UserID  int64 `json:"user_id,omitempty"`
	OrderID int64 `json:"order_id"`
}

type createPaymentResponse struct {
	PaymentID   int64  `json:"payment_id"`
	OrderID     int64  `json:"order_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CheckoutURL string `json:"checkout_url"`
	IsReplay    bool   `json:"is_replay"`
}

func (s *Server) handleCreatePayment(w http.ResponseWriter, r *http.Request) {
	// 5.1: Пользователь только из проверенного JWT в middleware; заголовок X-User-ID игнорируется
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		writeDomainError(w, domain.ErrUnauthorized)
		return
	}

	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		writeDomainError(w, domain.ErrInvalidIdempotencyKey)
		return
	}

	var req createPaymentRequest
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON request body")
		return
	}

	if req.OrderID <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "order_id must be a positive integer")
		return
	}

	result, err := s.paymentSvc.CreatePayment(r.Context(), service.CreatePaymentInput{
		UserID:         userID,
		OrderID:        req.OrderID,
		IdempotencyKey: idemKey,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}

	// 5.2: Никогда не 200 с пустым checkout_url (если сессия не готова — 409 с Retry-After)
	if result.CheckoutURL == "" {
		w.Header().Set("Retry-After", "1")
		writeDomainError(w, domain.ErrRequestInProgress)
		return
	}

	statusCode := http.StatusCreated
	if result.IsReplay {
		statusCode = http.StatusOK
	}

	writeJSON(w, statusCode, createPaymentResponse{
		PaymentID:   result.Payment.ID,
		OrderID:     result.Payment.OrderID,
		AmountMinor: result.Payment.AmountMinor,
		Currency:    result.Payment.Currency,
		Status:      string(result.Payment.Status),
		CheckoutURL: result.CheckoutURL,
		IsReplay:    result.IsReplay,
	})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// 1. Ограничение входящего тела до 64 КБ
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	// 2. Чтение сырых байт ДО парсинга JSON
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Failed to read request body")
		return
	}

	signature := strings.TrimSpace(r.Header.Get("X-Signature"))
	timestamp := strings.TrimSpace(r.Header.Get("X-Timestamp"))

	if signature == "" || timestamp == "" {
		s.logger.WarnContext(r.Context(), "missing required webhook security headers")
		writeError(w, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return
	}

	if err := s.paymentSvc.ProcessWebhook(r.Context(), rawBody, signature, timestamp); err != nil {
		if errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrExpiredTimestamp) {
			writeDomainError(w, err)
			return
		}

		if errors.Is(err, domain.ErrWebhookAmountMismatch) || errors.Is(err, domain.ErrProviderIDMismatch) {
			writeDomainError(w, err)
			return
		}

		// Временная ошибка на нашей стороне: возвращаем 5xx, чтобы провайдер повторил доставку
		s.logger.ErrorContext(r.Context(), "transient error processing webhook, requesting retry",
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "Transient processing error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.readyChecker != nil {
		if err := s.readyChecker(r.Context()); err != nil {
			s.logger.WarnContext(r.Context(), "readiness check failed", slog.String("error", err.Error()))
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
