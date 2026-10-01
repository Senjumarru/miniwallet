package domain

import "time"

const CurrencyKZT = "KZT"

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	IsActive  bool      `json:"is_active"`
	IsBlocked bool      `json:"is_blocked"`
	CreatedAt time.Time `json:"created_at"`
}

type OrderStatus string

const (
	OrderStatusUnpaid   OrderStatus = "unpaid"
	OrderStatusPaid     OrderStatus = "paid"
	OrderStatusCanceled OrderStatus = "canceled"
)

type Order struct {
	ID          int64       `json:"id"`
	UserID      int64       `json:"user_id"`
	AmountMinor int64       `json:"amount_minor"`
	Currency    string      `json:"currency"`
	Status      OrderStatus `json:"status"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type PaymentStatus string

const (
	PaymentStatusPending   PaymentStatus = "pending"
	PaymentStatusSucceeded PaymentStatus = "succeeded"
	PaymentStatusFailed    PaymentStatus = "failed"
	PaymentStatusCanceled  PaymentStatus = "canceled"
)

func (s PaymentStatus) IsFinal() bool {
	return s == PaymentStatusSucceeded || s == PaymentStatusFailed || s == PaymentStatusCanceled
}

// CanTransitionTo проверяет допустимость перехода статуса платежа.
// Финальные статусы (succeeded, failed, canceled) менять нельзя.
func (s PaymentStatus) CanTransitionTo(target PaymentStatus) bool {
	if s.IsFinal() {
		return false
	}
	switch s {
	case PaymentStatusPending:
		return target == PaymentStatusSucceeded || target == PaymentStatusFailed || target == PaymentStatusCanceled
	default:
		return false
	}
}

type Payment struct {
	ID                int64         `json:"id"`
	UserID            int64         `json:"user_id"`
	OrderID           int64         `json:"order_id"`
	AmountMinor       int64         `json:"amount_minor"`
	Currency          string        `json:"currency"`
	Status            PaymentStatus `json:"status"`
	IdempotencyKey    string        `json:"idempotency_key"`
	RequestHash       string        `json:"request_hash"`
	ProviderPaymentID string        `json:"provider_payment_id,omitempty"`
	CheckoutURL       string        `json:"checkout_url,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

type WebhookEvent struct {
	ID          int64     `json:"id"`
	EventID     string    `json:"event_id"`
	EventType   string    `json:"event_type"`
	Payload     []byte    `json:"payload"`
	ProcessedAt time.Time `json:"processed_at"`
}

// PaymentEvent фиксирует аудит любых изменений состояния платежей
type PaymentEvent struct {
	ID         int64     `json:"id"`
	PaymentID  int64     `json:"payment_id"`
	EventType  string    `json:"event_type"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status,omitempty"`
	Metadata   string    `json:"metadata,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// SecurityEvent фиксирует аудит событий безопасности (с привязкой к платежу или без)
type SecurityEvent struct {
	ID        int64     `json:"id"`
	PaymentID *int64    `json:"payment_id,omitempty"`
	EventType string    `json:"event_type"`
	Metadata  string    `json:"metadata,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
