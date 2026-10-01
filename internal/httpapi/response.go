package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/provider"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			slog.Error("failed to encode json response", slog.String("error", err.Error()))
		}
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{
		Error: apiError{
			Code:    code,
			Message: message,
		},
	})
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	// Безопасность: невалидная подпись или timestamp отдают 401 без раскрытия деталей
	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrInvalidSignature), errors.Is(err, domain.ErrExpiredTimestamp):
		writeError(w, http.StatusUnauthorized, "unauthorized", "Unauthorized")

	case errors.Is(err, domain.ErrRequestInProgress):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "request_in_progress", "Payment request is already in progress, please retry shortly")

	case errors.Is(err, domain.ErrProviderIDMismatch):
		writeError(w, http.StatusUnprocessableEntity, "provider_id_mismatch", "Provider payment ID mismatch")

	case errors.Is(err, domain.ErrIdempotencyConflict):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "idempotency_conflict", "Idempotency key has already been used with different parameters")

	case errors.Is(err, domain.ErrActivePaymentExists):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "active_payment_exists", "Active pending payment already exists for this order")

	case errors.Is(err, domain.ErrPaymentNotPayable):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "payment_not_payable", "Payment cannot be paid; please use a new Idempotency-Key")

	case errors.Is(err, provider.ErrCircuitOpen):
		writeError(w, http.StatusServiceUnavailable, "circuit_breaker_open", "Payment provider circuit breaker is open")

	case errors.Is(err, provider.ErrProviderOverloaded):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "provider_overloaded", "Payment provider is overloaded")

	case errors.Is(err, domain.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user_not_found", "User not found")

	case errors.Is(err, domain.ErrOrderNotFound):
		writeError(w, http.StatusNotFound, "order_not_found", "Order not found")

	case errors.Is(err, domain.ErrPaymentNotFound):
		writeError(w, http.StatusNotFound, "payment_not_found", "Payment not found")

	case errors.Is(err, domain.ErrOrderForbidden):
		writeError(w, http.StatusForbidden, "order_forbidden", "Order does not belong to the user")

	case errors.Is(err, domain.ErrInvalidIdempotencyKey):
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key header is missing or malformed")

	case errors.Is(err, domain.ErrUserInactive):
		writeError(w, http.StatusUnprocessableEntity, "user_inactive", "User account is inactive")

	case errors.Is(err, domain.ErrUserBlocked):
		writeError(w, http.StatusUnprocessableEntity, "user_blocked", "User account is blocked")

	case errors.Is(err, domain.ErrOrderNotUnpaid):
		writeError(w, http.StatusUnprocessableEntity, "order_not_unpaid", "Order is not in unpaid status")

	case errors.Is(err, domain.ErrOrderInvalidAmount):
		writeError(w, http.StatusUnprocessableEntity, "invalid_amount", "Order amount must be positive")

	case errors.Is(err, domain.ErrUnsupportedCurrency):
		writeError(w, http.StatusUnprocessableEntity, "unsupported_currency", "Currency is not supported")

	case errors.Is(err, domain.ErrWebhookAmountMismatch):
		writeError(w, http.StatusUnprocessableEntity, "amount_mismatch", "Webhook amount or currency mismatch")

	case errors.Is(err, provider.ErrProviderUnavailable):
		writeError(w, http.StatusBadGateway, "provider_unavailable", "Payment provider is currently unavailable")

	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred")
	}
}
