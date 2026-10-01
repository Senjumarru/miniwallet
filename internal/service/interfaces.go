package service

import (
	"context"
	"database/sql"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

type UserRepository interface {
	GetByID(ctx context.Context, id int64) (*domain.User, error)
}

type OrderRepository interface {
	GetByID(ctx context.Context, id int64) (*domain.Order, error)
	GetByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*domain.Order, error)
	UpdateStatusTx(ctx context.Context, tx *sql.Tx, orderID int64, fromStatus, toStatus domain.OrderStatus) error
}

type PaymentRepository interface {
	GetByID(ctx context.Context, id int64) (*domain.Payment, error)
	GetByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*domain.Payment, error)
	GetByIdempotencyKey(ctx context.Context, userID int64, key string) (*domain.Payment, error)
	GetActivePendingByOrderID(ctx context.Context, orderID int64) (*domain.Payment, error)
	GetPendingOlderThan(ctx context.Context, olderThan time.Time, limit int) ([]*domain.Payment, error)
	CreatePending(ctx context.Context, p *domain.Payment) error
	UpdateSession(ctx context.Context, paymentID int64, providerPaymentID, checkoutURL string) error
	UpdateSessionTx(ctx context.Context, tx *sql.Tx, paymentID int64, providerPaymentID, checkoutURL string) error
	UpdateStatus(ctx context.Context, paymentID int64, fromStatus, toStatus domain.PaymentStatus) error
	UpdateStatusTx(ctx context.Context, tx *sql.Tx, paymentID int64, fromStatus, toStatus domain.PaymentStatus) error
}

type WebhookEventRepository interface {
	RecordEventTx(ctx context.Context, tx *sql.Tx, eventID, eventType string, payload []byte) (isDuplicate bool, err error)
}

type PaymentEventRepository interface {
	RecordEventTx(ctx context.Context, tx *sql.Tx, e domain.PaymentEvent) error
	ListByPaymentID(ctx context.Context, paymentID int64) ([]domain.PaymentEvent, error)
	GetManualReviewEvents(ctx context.Context) ([]domain.PaymentEvent, error)
}

type SecurityEventRepository interface {
	RecordSecurityEvent(ctx context.Context, e domain.SecurityEvent) error
	RecordSecurityEventTx(ctx context.Context, tx *sql.Tx, e domain.SecurityEvent) error
	ListSecurityEvents(ctx context.Context) ([]domain.SecurityEvent, error)
}

type TxManager interface {
	WithinTransaction(ctx context.Context, fn func(txCtx context.Context, tx *sql.Tx) error) error
}
