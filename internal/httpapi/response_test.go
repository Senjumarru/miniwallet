package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/provider"
)

func TestWriteDomainError_Table(t *testing.T) {
	tests := []struct {
		name               string
		err                error
		expectedStatus     int
		expectedCode       string
		expectedRetryAfter string
	}{
		{
			name:           "ErrUnauthorized",
			err:            domain.ErrUnauthorized,
			expectedStatus: http.StatusUnauthorized,
			expectedCode:   "unauthorized",
		},
		{
			name:           "ErrInvalidSignature",
			err:            domain.ErrInvalidSignature,
			expectedStatus: http.StatusUnauthorized,
			expectedCode:   "unauthorized",
		},
		{
			name:           "ErrExpiredTimestamp",
			err:            domain.ErrExpiredTimestamp,
			expectedStatus: http.StatusUnauthorized,
			expectedCode:   "unauthorized",
		},
		{
			name:               "ErrRequestInProgress",
			err:                domain.ErrRequestInProgress,
			expectedStatus:     http.StatusConflict,
			expectedCode:       "request_in_progress",
			expectedRetryAfter: "1",
		},
		{
			name:           "ErrProviderIDMismatch",
			err:            domain.ErrProviderIDMismatch,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "provider_id_mismatch",
		},
		{
			name:               "ErrIdempotencyConflict",
			err:                domain.ErrIdempotencyConflict,
			expectedStatus:     http.StatusConflict,
			expectedCode:       "idempotency_conflict",
			expectedRetryAfter: "1",
		},
		{
			name:               "ErrActivePaymentExists",
			err:                domain.ErrActivePaymentExists,
			expectedStatus:     http.StatusConflict,
			expectedCode:       "active_payment_exists",
			expectedRetryAfter: "1",
		},
		{
			name:               "ErrPaymentNotPayable",
			err:                domain.ErrPaymentNotPayable,
			expectedStatus:     http.StatusConflict,
			expectedCode:       "payment_not_payable",
			expectedRetryAfter: "1",
		},
		{
			name:           "ErrCircuitOpen",
			err:            provider.ErrCircuitOpen,
			expectedStatus: http.StatusServiceUnavailable,
			expectedCode:   "circuit_breaker_open",
		},
		{
			name:               "ErrProviderOverloaded",
			err:                provider.ErrProviderOverloaded,
			expectedStatus:     http.StatusServiceUnavailable,
			expectedCode:       "provider_overloaded",
			expectedRetryAfter: "1",
		},
		{
			name:           "ErrUserNotFound",
			err:            domain.ErrUserNotFound,
			expectedStatus: http.StatusNotFound,
			expectedCode:   "user_not_found",
		},
		{
			name:           "ErrOrderNotFound",
			err:            domain.ErrOrderNotFound,
			expectedStatus: http.StatusNotFound,
			expectedCode:   "order_not_found",
		},
		{
			name:           "ErrPaymentNotFound",
			err:            domain.ErrPaymentNotFound,
			expectedStatus: http.StatusNotFound,
			expectedCode:   "payment_not_found",
		},
		{
			name:           "ErrOrderForbidden",
			err:            domain.ErrOrderForbidden,
			expectedStatus: http.StatusForbidden,
			expectedCode:   "order_forbidden",
		},
		{
			name:           "ErrInvalidIdempotencyKey",
			err:            domain.ErrInvalidIdempotencyKey,
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "invalid_idempotency_key",
		},
		{
			name:           "ErrUserInactive",
			err:            domain.ErrUserInactive,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "user_inactive",
		},
		{
			name:           "ErrUserBlocked",
			err:            domain.ErrUserBlocked,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "user_blocked",
		},
		{
			name:           "ErrOrderNotUnpaid",
			err:            domain.ErrOrderNotUnpaid,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "order_not_unpaid",
		},
		{
			name:           "ErrOrderInvalidAmount",
			err:            domain.ErrOrderInvalidAmount,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "invalid_amount",
		},
		{
			name:           "ErrUnsupportedCurrency",
			err:            domain.ErrUnsupportedCurrency,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "unsupported_currency",
		},
		{
			name:           "ErrWebhookAmountMismatch",
			err:            domain.ErrWebhookAmountMismatch,
			expectedStatus: http.StatusUnprocessableEntity,
			expectedCode:   "amount_mismatch",
		},
		{
			name:           "ErrProviderUnavailable",
			err:            provider.ErrProviderUnavailable,
			expectedStatus: http.StatusBadGateway,
			expectedCode:   "provider_unavailable",
		},
		{
			name:           "ErrAmbiguousOutcome",
			err:            provider.ErrAmbiguousOutcome,
			expectedStatus: http.StatusBadGateway,
			expectedCode:   "provider_unavailable",
		},
		{
			name:           "ErrDeadlineExceeded",
			err:            context.DeadlineExceeded,
			expectedStatus: http.StatusGatewayTimeout,
			expectedCode:   "gateway_timeout",
		},
		{
			name:           "ErrDefinitiveRejection",
			err:            provider.ErrDefinitiveRejection,
			expectedStatus: http.StatusBadGateway,
			expectedCode:   "provider_rejected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeDomainError(w, tc.err)

			if w.Code != tc.expectedStatus {
				t.Fatalf("expected status %d, got %d", tc.expectedStatus, w.Code)
			}

			if tc.expectedRetryAfter != "" {
				if actualRetry := w.Header().Get("Retry-After"); actualRetry != tc.expectedRetryAfter {
					t.Fatalf("expected Retry-After %s, got %s", tc.expectedRetryAfter, actualRetry)
				}
			}

			var env errorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal error envelope: %v", err)
			}
			if env.Error.Code != tc.expectedCode {
				t.Fatalf("expected error code %s, got %s", tc.expectedCode, env.Error.Code)
			}
		})
	}
}

func TestWriteDomainError_UnknownError_500WithoutDetails(t *testing.T) {
	w := httptest.NewRecorder()
	secretDBErrorMessage := "FATAL: database disk image is malformed at offset 0x48f029a in table payments"
	unknownErr := errors.New(secretDBErrorMessage)

	writeDomainError(w, unknownErr)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error, got %d", w.Code)
	}

	body := w.Body.String()
	if strings.Contains(body, secretDBErrorMessage) || strings.Contains(body, "malformed") {
		t.Fatalf("security violation: internal error details leaked in response body: %s", body)
	}

	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}

	if env.Error.Code != "internal_error" {
		t.Errorf("expected error code 'internal_error', got %s", env.Error.Code)
	}
	if env.Error.Message != "An internal error occurred" {
		t.Errorf("expected generic error message, got %s", env.Error.Message)
	}
}
