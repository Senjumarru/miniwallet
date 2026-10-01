package domain

import "errors"

// Sentinel-ошибки домена (проверяются через errors.Is)
var (
	ErrUnauthorized          = errors.New("unauthorized")
	ErrUserNotFound          = errors.New("user not found")
	ErrUserInactive          = errors.New("user is inactive")
	ErrUserBlocked           = errors.New("user is blocked")
	ErrOrderNotFound         = errors.New("order not found")
	ErrOrderForbidden        = errors.New("order belongs to another user")
	ErrOrderNotUnpaid        = errors.New("order is not in unpaid status")
	ErrOrderInvalidAmount    = errors.New("order amount must be greater than zero")
	ErrUnsupportedCurrency   = errors.New("unsupported currency")
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key format")
	ErrIdempotencyConflict   = errors.New("idempotency key already used with different parameters")
	ErrRequestInProgress     = errors.New("payment request in progress")
	ErrActivePaymentExists   = errors.New("active pending payment already exists for this order")
	ErrPaymentNotFound       = errors.New("payment not found")
	ErrInvalidTransition     = errors.New("invalid payment status transition")
	ErrInvalidSignature      = errors.New("invalid webhook signature")
	ErrExpiredTimestamp      = errors.New("webhook timestamp expired or out of allowed window")
	ErrWebhookAmountMismatch = errors.New("webhook amount or currency mismatch with order")
	ErrProviderIDMismatch    = errors.New("webhook provider_payment_id does not match payment session")
	ErrStatusConflict        = errors.New("status conflict")
	ErrPaymentNotPayable     = errors.New("payment is not payable: use a new idempotency key")
)

// ErrUniqueViolation представляет нарушение ограничения уникальности в хранилище.
type ErrUniqueViolation struct {
	Constraint string // "order_pending" | "idempotency_key" | "order_succeeded"
}

func (e *ErrUniqueViolation) Error() string {
	return "unique violation: " + e.Constraint
}
