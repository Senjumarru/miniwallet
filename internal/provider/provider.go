package provider

import (
	"context"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

type CreateCheckoutInput struct {
	PaymentID      int64  `json:"payment_id"`
	OrderID        int64  `json:"order_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CheckoutSession struct {
	ProviderPaymentID string `json:"provider_payment_id"`
	CheckoutURL       string `json:"checkout_url"`
}

// PaymentProvider — интерфейс взаимодействия со сторонним платёжным шлюзом.
type PaymentProvider interface {
	CreateCheckoutSession(ctx context.Context, in CreateCheckoutInput) (CheckoutSession, error)
	GetPaymentStatus(ctx context.Context, providerPaymentID string) (domain.PaymentStatus, error)
}
