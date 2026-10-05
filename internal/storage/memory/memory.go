package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

type Storage struct {
	txMu          sync.Mutex
	mu            sync.RWMutex
	clock         domain.Clock
	users         map[int64]*domain.User
	orders        map[int64]*domain.Order
	payments      map[int64]*domain.Payment
	paymentEvents map[int64][]domain.PaymentEvent
	securityEvts  []domain.SecurityEvent
	webhookEvts   map[string]struct{}
	userKeyIndex  map[string]int64
	nextPayID     int64
	nextEventID   int64
}

type storageSnapshot struct {
	users         map[int64]*domain.User
	orders        map[int64]*domain.Order
	payments      map[int64]*domain.Payment
	paymentEvents map[int64][]domain.PaymentEvent
	securityEvts  []domain.SecurityEvent
	webhookEvts   map[string]struct{}
	userKeyIndex  map[string]int64
	nextPayID     int64
	nextEventID   int64
}

func (s *Storage) snapshot() *storageSnapshot {
	snap := &storageSnapshot{
		users:         make(map[int64]*domain.User, len(s.users)),
		orders:        make(map[int64]*domain.Order, len(s.orders)),
		payments:      make(map[int64]*domain.Payment, len(s.payments)),
		paymentEvents: make(map[int64][]domain.PaymentEvent, len(s.paymentEvents)),
		securityEvts:  make([]domain.SecurityEvent, len(s.securityEvts)),
		webhookEvts:   make(map[string]struct{}, len(s.webhookEvts)),
		userKeyIndex:  make(map[string]int64, len(s.userKeyIndex)),
		nextPayID:     s.nextPayID,
		nextEventID:   s.nextEventID,
	}

	for k, v := range s.users {
		cp := *v
		snap.users[k] = &cp
	}
	for k, v := range s.orders {
		cp := *v
		snap.orders[k] = &cp
	}
	for k, v := range s.payments {
		cp := *v
		snap.payments[k] = &cp
	}
	for k, v := range s.paymentEvents {
		evts := make([]domain.PaymentEvent, len(v))
		copy(evts, v)
		snap.paymentEvents[k] = evts
	}
	copy(snap.securityEvts, s.securityEvts)
	for k := range s.webhookEvts {
		snap.webhookEvts[k] = struct{}{}
	}
	for k, v := range s.userKeyIndex {
		snap.userKeyIndex[k] = v
	}
	return snap
}

func (s *Storage) restore(snap *storageSnapshot) {
	s.users = snap.users
	s.orders = snap.orders
	s.payments = snap.payments
	s.paymentEvents = snap.paymentEvents
	s.securityEvts = snap.securityEvts
	s.webhookEvts = snap.webhookEvts
	s.userKeyIndex = snap.userKeyIndex
	s.nextPayID = snap.nextPayID
	s.nextEventID = snap.nextEventID
}

func (s *Storage) WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	s.mu.Lock()
	snap := s.snapshot()
	s.mu.Unlock()

	if err := fn(ctx); err != nil {
		s.mu.Lock()
		s.restore(snap)
		s.mu.Unlock()
		return err
	}
	return nil
}

func New() *Storage {
	return &Storage{
		clock:         domain.RealClock{},
		users:         make(map[int64]*domain.User),
		orders:        make(map[int64]*domain.Order),
		payments:      make(map[int64]*domain.Payment),
		paymentEvents: make(map[int64][]domain.PaymentEvent),
		webhookEvts:   make(map[string]struct{}),
		userKeyIndex:  make(map[string]int64),
		nextPayID:     1,
		nextEventID:   1,
	}
}

func (s *Storage) SetClock(c domain.Clock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = c
}

func (s *Storage) now() time.Time {
	if s.clock != nil {
		return s.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Storage) SeedUser(u *domain.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
}

func (s *Storage) SeedOrder(o *domain.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.ID] = o
}

func (s *Storage) Users() *UserRepo {
	return &UserRepo{s: s}
}

func (s *Storage) Orders() *OrderRepo {
	return &OrderRepo{s: s}
}

func (s *Storage) Payments() *PaymentRepo {
	return &PaymentRepo{s: s}
}

func (s *Storage) Webhooks() *WebhookRepo {
	return &WebhookRepo{s: s}
}

func (s *Storage) PaymentEvents() *PaymentEventRepo {
	return &PaymentEventRepo{s: s}
}

func (s *Storage) SecurityEvents() *SecurityEventRepo {
	return &SecurityEventRepo{s: s}
}

// UserRepo
type UserRepo struct{ s *Storage }

func (r *UserRepo) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	u, ok := r.s.users[id]
	if !ok {
		return nil, domain.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

// OrderRepo
type OrderRepo struct{ s *Storage }

func (r *OrderRepo) GetByID(ctx context.Context, id int64) (*domain.Order, error) {
	return r.GetByIDTx(ctx, nil, id)
}

func (r *OrderRepo) GetByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*domain.Order, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	o, ok := r.s.orders[id]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	cp := *o
	return &cp, nil
}

func (r *OrderRepo) UpdateStatus(ctx context.Context, orderID int64, fromStatus, toStatus domain.OrderStatus) error {
	return r.UpdateStatusTx(ctx, nil, orderID, fromStatus, toStatus)
}

func (r *OrderRepo) UpdateStatusTx(ctx context.Context, tx *sql.Tx, orderID int64, fromStatus, toStatus domain.OrderStatus) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	o, ok := r.s.orders[orderID]
	if !ok {
		return domain.ErrOrderNotFound
	}
	if o.Status != fromStatus {
		return domain.ErrStatusConflict
	}
	o.Status = toStatus
	o.UpdatedAt = r.s.now()
	return nil
}

// PaymentRepo
type PaymentRepo struct{ s *Storage }

func (r *PaymentRepo) GetByID(ctx context.Context, id int64) (*domain.Payment, error) {
	return r.GetByIDTx(ctx, nil, id)
}

func (r *PaymentRepo) GetByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*domain.Payment, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	p, ok := r.s.payments[id]
	if !ok {
		return nil, domain.ErrPaymentNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *PaymentRepo) GetByIdempotencyKey(ctx context.Context, userID int64, key string) (*domain.Payment, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	idxKey := fmt.Sprintf("%d:%s", userID, key)
	payID, ok := r.s.userKeyIndex[idxKey]
	if !ok {
		return nil, domain.ErrPaymentNotFound
	}
	p := r.s.payments[payID]
	cp := *p
	return &cp, nil
}

func (r *PaymentRepo) GetActivePendingByOrderID(ctx context.Context, orderID int64) (*domain.Payment, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	for _, p := range r.s.payments {
		if p.OrderID == orderID && p.Status == domain.PaymentStatusPending {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.ErrPaymentNotFound
}

func (r *PaymentRepo) GetPendingOlderThan(ctx context.Context, olderThan time.Time, limit int) ([]*domain.Payment, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	var list []*domain.Payment
	for _, p := range r.s.payments {
		if p.Status == domain.PaymentStatusPending && p.CreatedAt.Before(olderThan.UTC()) {
			cp := *p
			list = append(list, &cp)
			if limit > 0 && len(list) >= limit {
				break
			}
		}
	}
	return list, nil
}

func (r *PaymentRepo) CreatePending(ctx context.Context, p *domain.Payment) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	idxKey := fmt.Sprintf("%d:%s", p.UserID, p.IdempotencyKey)
	if _, exists := r.s.userKeyIndex[idxKey]; exists {
		return &domain.ErrUniqueViolation{Constraint: "idempotency_key"}
	}

	for _, existing := range r.s.payments {
		if existing.OrderID == p.OrderID && existing.Status == domain.PaymentStatusPending {
			return &domain.ErrUniqueViolation{Constraint: "order_pending"}
		}
	}

	p.ID = r.s.nextPayID
	r.s.nextPayID++
	p.Status = domain.PaymentStatusPending
	if p.CreatedAt.IsZero() {
		p.CreatedAt = r.s.now()
	}
	p.UpdatedAt = r.s.now()

	cp := *p
	r.s.payments[p.ID] = &cp
	r.s.userKeyIndex[idxKey] = p.ID
	return nil
}

func (r *PaymentRepo) UpdateSession(ctx context.Context, paymentID int64, providerPaymentID, checkoutURL string) error {
	return r.UpdateSessionTx(ctx, nil, paymentID, providerPaymentID, checkoutURL)
}

func (r *PaymentRepo) UpdateSessionTx(ctx context.Context, tx *sql.Tx, paymentID int64, providerPaymentID, checkoutURL string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	p, ok := r.s.payments[paymentID]
	if !ok {
		return domain.ErrPaymentNotFound
	}
	if p.Status != domain.PaymentStatusPending {
		return domain.ErrStatusConflict
	}
	p.ProviderPaymentID = providerPaymentID
	p.CheckoutURL = checkoutURL
	p.UpdatedAt = r.s.now()
	return nil
}

func (r *PaymentRepo) UpdateStatus(ctx context.Context, paymentID int64, fromStatus, toStatus domain.PaymentStatus) error {
	return r.UpdateStatusTx(ctx, nil, paymentID, fromStatus, toStatus)
}

func (r *PaymentRepo) UpdateStatusTx(ctx context.Context, tx *sql.Tx, paymentID int64, fromStatus, toStatus domain.PaymentStatus) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	p, ok := r.s.payments[paymentID]
	if !ok {
		return domain.ErrPaymentNotFound
	}
	if p.Status != fromStatus {
		return domain.ErrStatusConflict
	}
	p.Status = toStatus
	p.UpdatedAt = r.s.now()
	return nil
}

// WebhookRepo
type WebhookRepo struct{ s *Storage }

func (r *WebhookRepo) RecordEvent(ctx context.Context, eventID, eventType string, payload []byte) (bool, error) {
	return r.RecordEventTx(ctx, nil, eventID, eventType, payload)
}

func (r *WebhookRepo) RecordEventTx(ctx context.Context, tx *sql.Tx, eventID, eventType string, payload []byte) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	if _, exists := r.s.webhookEvts[eventID]; exists {
		return true, nil
	}
	r.s.webhookEvts[eventID] = struct{}{}
	return false, nil
}

// PaymentEventRepo
type PaymentEventRepo struct{ s *Storage }

func (r *PaymentEventRepo) RecordEvent(ctx context.Context, e domain.PaymentEvent) error {
	return r.RecordEventTx(ctx, nil, e)
}

func (r *PaymentEventRepo) RecordEventTx(ctx context.Context, tx *sql.Tx, e domain.PaymentEvent) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	e.ID = r.s.nextEventID
	r.s.nextEventID++
	if e.CreatedAt.IsZero() {
		e.CreatedAt = r.s.now()
	}
	r.s.paymentEvents[e.PaymentID] = append(r.s.paymentEvents[e.PaymentID], e)
	return nil
}

func (r *PaymentEventRepo) ListByPaymentID(ctx context.Context, paymentID int64) ([]domain.PaymentEvent, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	evts, ok := r.s.paymentEvents[paymentID]
	if !ok {
		return nil, nil
	}
	res := make([]domain.PaymentEvent, len(evts))
	copy(res, evts)
	return res, nil
}

func (r *PaymentEventRepo) GetManualReviewEvents(ctx context.Context) ([]domain.PaymentEvent, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	var res []domain.PaymentEvent
	for _, evts := range r.s.paymentEvents {
		for _, e := range evts {
			if e.EventType == "payment.late_success_requires_refund" || e.EventType == "payment.duplicate_requires_refund" {
				res = append(res, e)
			}
		}
	}
	return res, nil
}

// SecurityEventRepo
type SecurityEventRepo struct{ s *Storage }

func (r *SecurityEventRepo) RecordSecurityEvent(ctx context.Context, e domain.SecurityEvent) error {
	return r.RecordSecurityEventTx(ctx, nil, e)
}

func (r *SecurityEventRepo) RecordSecurityEventTx(ctx context.Context, tx *sql.Tx, e domain.SecurityEvent) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()

	e.ID = int64(len(r.s.securityEvts) + 1)
	if e.CreatedAt.IsZero() {
		e.CreatedAt = r.s.now()
	}
	r.s.securityEvts = append(r.s.securityEvts, e)
	return nil
}

func (r *SecurityEventRepo) ListSecurityEvents(ctx context.Context) ([]domain.SecurityEvent, error) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()

	res := make([]domain.SecurityEvent, len(r.s.securityEvts))
	copy(res, r.s.securityEvts)
	return res, nil
}
