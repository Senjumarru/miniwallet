package service

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/provider"
)

type ReconcilerConfig struct {
	TTL      time.Duration // время жизни pending-платежа до сверки
	Interval time.Duration // периодичность запуска воркера
	Batch    int           // количество платежей за одну итерацию
}

type Reconciler struct {
	payments      PaymentRepository
	orders        OrderRepository
	paymentEvents PaymentEventRepository
	txManager     TxManager
	provider      provider.PaymentProvider
	logger        *slog.Logger
	clock         domain.Clock
	cfg           ReconcilerConfig
}

func NewReconciler(
	payments PaymentRepository,
	orders OrderRepository,
	paymentEvents PaymentEventRepository,
	txManager TxManager,
	provider provider.PaymentProvider,
	logger *slog.Logger,
	cfg ReconcilerConfig,
) *Reconciler {
	if cfg.TTL <= 0 {
		cfg.TTL = 15 * time.Minute
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 1 * time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	return &Reconciler{
		payments:      payments,
		orders:        orders,
		paymentEvents: paymentEvents,
		txManager:     txManager,
		provider:      provider,
		logger:        logger,
		clock:         domain.RealClock{},
		cfg:           cfg,
	}
}

func (r *Reconciler) SetClock(clock domain.Clock) {
	r.clock = clock
}

func (r *Reconciler) now() time.Time {
	if r.clock != nil {
		return r.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Reconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	r.logger.Info("payment reconciler worker started",
		slog.Duration("interval", r.cfg.Interval),
		slog.Duration("ttl", r.cfg.TTL),
	)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("payment reconciler worker stopped")
			return
		case <-ticker.C:
			count, err := r.ReconcileOnce(ctx)
			if err != nil {
				r.logger.Error("reconciler iteration failed", slog.String("error", err.Error()))
			} else if count > 0 {
				r.logger.Info("reconciler completed run", slog.Int("reconciled_count", count))
			}
		}
	}
}

func (r *Reconciler) ReconcileOnce(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// 4.2: Clock в тестах, ограничение пакета
	threshold := r.now().Add(-r.cfg.TTL)
	staleList, err := r.payments.GetPendingOlderThan(ctx, threshold, r.cfg.Batch)
	if err != nil {
		return 0, fmt.Errorf("fetch stale pending payments: %w", err)
	}

	reconciled := 0
	for _, p := range staleList {
		// 4.2: Корректная остановка по ctx
		if err := ctx.Err(); err != nil {
			return reconciled, err
		}

		// 4.2: Ошибка по одному платежу не останавливает цикл
		if err := r.reconcilePayment(ctx, p); err != nil {
			r.logger.Error("failed to reconcile payment",
				slog.Int64("payment_id", p.ID),
				slog.Int64("order_id", p.OrderID),
				slog.String("error", err.Error()),
			)
			continue
		}
		reconciled++
	}

	return reconciled, nil
}

func (r *Reconciler) reconcilePayment(ctx context.Context, p *domain.Payment) error {
	// 4.1: Перед переводом pending в failed ВСЕГДА спрашивай провайдера (по ключу pay-<id>), в том числе при пустом provider_payment_id
	lookupID := p.ProviderPaymentID
	if lookupID == "" {
		lookupID = fmt.Sprintf("pay-%d", p.ID)
	}

	provStatus, err := r.provider.GetPaymentStatus(ctx, lookupID)
	if err != nil {
		// Ошибка обращения к провайдеру: НЕ переводим в failed, оставляем pending для следующей сверки
		return fmt.Errorf("check provider status for %s: %w", lookupID, err)
	}

	return r.txManager.WithinTransaction(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		switch provStatus {
		case domain.PaymentStatusSucceeded:
			// Если сессия не была привязана, привязываем
			if p.ProviderPaymentID == "" {
				if err := r.payments.UpdateSessionTx(txCtx, tx, p.ID, lookupID, p.CheckoutURL); err != nil {
					return fmt.Errorf("attach provider payment id: %w", err)
				}
			}
			if err := r.payments.UpdateStatusTx(txCtx, tx, p.ID, domain.PaymentStatusPending, domain.PaymentStatusSucceeded); err != nil {
				return err
			}
			if err := r.orders.UpdateStatusTx(txCtx, tx, p.OrderID, domain.OrderStatusUnpaid, domain.OrderStatusPaid); err != nil {
				return err
			}
			return r.paymentEvents.RecordEventTx(txCtx, tx, domain.PaymentEvent{
				PaymentID:  p.ID,
				EventType:  "reconciliation.provider_succeeded",
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(domain.PaymentStatusSucceeded),
				Metadata:   fmt.Sprintf(`{"lookup_id":%q,"provider_status":"succeeded"}`, lookupID),
			})

		case domain.PaymentStatusFailed, domain.PaymentStatusCanceled:
			targetStatus := provStatus
			if err := r.payments.UpdateStatusTx(txCtx, tx, p.ID, domain.PaymentStatusPending, targetStatus); err != nil {
				return err
			}
			return r.paymentEvents.RecordEventTx(txCtx, tx, domain.PaymentEvent{
				PaymentID:  p.ID,
				EventType:  fmt.Sprintf("reconciliation.provider_%s", targetStatus),
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(targetStatus),
				Metadata:   fmt.Sprintf(`{"lookup_id":%q,"provider_status":"%s"}`, lookupID, targetStatus),
			})

		default:
			// Провайдер всё ещё сообщает pending, но локальный TTL истёк
			// TODO(docs): API провайдера для отмены сессии по истечении TTL / сессия провайдера не живёт дольше TTL.
			if err := r.payments.UpdateStatusTx(txCtx, tx, p.ID, domain.PaymentStatusPending, domain.PaymentStatusFailed); err != nil {
				return err
			}
			return r.paymentEvents.RecordEventTx(txCtx, tx, domain.PaymentEvent{
				PaymentID:  p.ID,
				EventType:  "reconciliation.expired",
				FromStatus: string(domain.PaymentStatusPending),
				ToStatus:   string(domain.PaymentStatusFailed),
				Metadata:   fmt.Sprintf(`{"lookup_id":%q,"reason":"expired_by_ttl"}`, lookupID),
			})
		}
	})
}
