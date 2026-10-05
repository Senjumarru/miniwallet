package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

// ClassifySQLiteError classifies a SQLite error, mapping ONLY unique/primary key violations
// to domain.ErrUniqueViolation (white list per Invariant 8).
func ClassifySQLiteError(err error) error {
	return classifySQLiteError(err)
}

func classifySQLiteError(err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code()
		errMsg := sqliteErr.Error()

		// Инвариант 8: ErrUniqueViolation только для UNIQUE/PRIMARYKEY (белый список).
		// Прочие ошибки БД (NOT NULL, CHECK, FOREIGN KEY и т.д.) это внутренние ошибки,
		// а не "заказ занят".
		isUnique := code == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
			code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY ||
			(code == sqlite3.SQLITE_CONSTRAINT && (strings.Contains(errMsg, "UNIQUE") || strings.Contains(errMsg, "PRIMARY KEY")))

		if !isUnique {
			return err
		}

		if strings.Contains(errMsg, "idx_payments_order_single_succeeded") {
			return &domain.ErrUniqueViolation{Constraint: "order_succeeded"}
		}
		if strings.Contains(errMsg, "idx_payments_order_single_pending") || strings.Contains(errMsg, "payments.order_id") {
			return &domain.ErrUniqueViolation{Constraint: "order_pending"}
		}
		if strings.Contains(errMsg, "uq_user_idempotency") || strings.Contains(errMsg, "payments.user_id") || strings.Contains(errMsg, "payments.idempotency_key") {
			return &domain.ErrUniqueViolation{Constraint: "idempotency_key"}
		}
		return &domain.ErrUniqueViolation{Constraint: "unknown"}
	}
	return err
}

type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getExecutor(ctx context.Context, db *sql.DB, explicitTx *sql.Tx) dbExecutor {
	if explicitTx != nil {
		return explicitTx
	}
	if tx := TxFromContext(ctx); tx != nil {
		return tx
	}
	return db
}

type UserRepository struct {
	db *sql.DB
}

func NewUserRepository(db *sql.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	exec := getExecutor(ctx, r.db, nil)
	row := exec.QueryRowContext(ctx, `
			SELECT id, email, is_active, is_blocked, created_at
			FROM users
			WHERE id = ?`, id)

	var u domain.User
	var createdAt string
	var isActive, isBlocked int
	err := row.Scan(&u.ID, &u.Email, &isActive, &isBlocked, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrUserNotFound
		}
		return nil, fmt.Errorf("query user: %w", err)
	}

	u.IsActive = isActive == 1
	u.IsBlocked = isBlocked == 1
	parsedCreated, err := parseDBTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse user created_at: %w", err)
	}
	u.CreatedAt = parsedCreated
	return &u, nil
}

type OrderRepository struct {
	db *sql.DB
}

func NewOrderRepository(db *sql.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

func (r *OrderRepository) GetByID(ctx context.Context, id int64) (*domain.Order, error) {
	return r.GetByIDTx(ctx, nil, id)
}

func (r *OrderRepository) GetByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*domain.Order, error) {
	exec := getExecutor(ctx, r.db, tx)
	row := exec.QueryRowContext(ctx, `
			SELECT id, user_id, amount_minor, currency, status, created_at, updated_at
			FROM orders
			WHERE id = ?`, id)

	return scanOrder(row)
}

func (r *OrderRepository) UpdateStatus(ctx context.Context, orderID int64, fromStatus, toStatus domain.OrderStatus) error {
	return r.UpdateStatusTx(ctx, nil, orderID, fromStatus, toStatus)
}

func (r *OrderRepository) UpdateStatusTx(ctx context.Context, tx *sql.Tx, orderID int64, fromStatus, toStatus domain.OrderStatus) error {
	exec := getExecutor(ctx, r.db, tx)
	res, err := exec.ExecContext(ctx, `
			UPDATE orders
			SET status = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND status = ?`,
		string(toStatus), orderID, string(fromStatus))
	if err != nil {
		return fmt.Errorf("update order status: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected rows: %w", err)
	}
	if affected == 0 {
		var dummy int64
		checkErr := exec.QueryRowContext(ctx, "SELECT id FROM orders WHERE id = ?", orderID).Scan(&dummy)
		if checkErr != nil {
			if errors.Is(checkErr, sql.ErrNoRows) {
				return domain.ErrOrderNotFound
			}
			return fmt.Errorf("check order existence: %w", checkErr)
		}
		return domain.ErrStatusConflict
	}
	return nil
}

type PaymentRepository struct {
	db *sql.DB
}

func NewPaymentRepository(db *sql.DB) *PaymentRepository {
	return &PaymentRepository{db: db}
}

func (r *PaymentRepository) GetByID(ctx context.Context, id int64) (*domain.Payment, error) {
	return r.GetByIDTx(ctx, nil, id)
}

func (r *PaymentRepository) GetByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*domain.Payment, error) {
	exec := getExecutor(ctx, r.db, tx)
	row := exec.QueryRowContext(ctx, `
			SELECT id, user_id, order_id, amount_minor, currency, status,
				idempotency_key, request_hash, provider_payment_id, checkout_url,
				created_at, updated_at
			FROM payments
			WHERE id = ?`, id)

	return scanPayment(row)
}

func (r *PaymentRepository) GetByIdempotencyKey(ctx context.Context, userID int64, key string) (*domain.Payment, error) {
	exec := getExecutor(ctx, r.db, nil)
	row := exec.QueryRowContext(ctx, `
			SELECT id, user_id, order_id, amount_minor, currency, status,
				idempotency_key, request_hash, provider_payment_id, checkout_url,
				created_at, updated_at
			FROM payments
			WHERE user_id = ? AND idempotency_key = ?`, userID, key)

	return scanPayment(row)
}

func (r *PaymentRepository) GetActivePendingByOrderID(ctx context.Context, orderID int64) (*domain.Payment, error) {
	exec := getExecutor(ctx, r.db, nil)
	row := exec.QueryRowContext(ctx, `
			SELECT id, user_id, order_id, amount_minor, currency, status,
				idempotency_key, request_hash, provider_payment_id, checkout_url,
				created_at, updated_at
			FROM payments
			WHERE order_id = ? AND status = 'pending'`, orderID)

	return scanPayment(row)
}

func (r *PaymentRepository) GetPendingOlderThan(ctx context.Context, olderThan time.Time, limit int) ([]*domain.Payment, error) {
	if limit <= 0 {
		limit = 50
	}
	formatted := olderThan.UTC().Format("2006-01-02T15:04:05.000Z")
	exec := getExecutor(ctx, r.db, nil)
	rows, err := exec.QueryContext(ctx, `
			SELECT id, user_id, order_id, amount_minor, currency, status,
				idempotency_key, request_hash, provider_payment_id, checkout_url,
				created_at, updated_at
			FROM payments
			WHERE status = 'pending' AND created_at < ?
			ORDER BY created_at ASC
			LIMIT ?`, formatted, limit)
	if err != nil {
		return nil, fmt.Errorf("query stale pending: %w", err)
	}
	defer rows.Close()

	var list []*domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (r *PaymentRepository) CreatePending(ctx context.Context, p *domain.Payment) error {
	query := `
			INSERT INTO payments (
				user_id, order_id, amount_minor, currency, status,
				idempotency_key, request_hash
			) VALUES (?, ?, ?, ?, 'pending', ?, ?)`
	args := []any{
		p.UserID, p.OrderID, p.AmountMinor, p.Currency,
		p.IdempotencyKey, p.RequestHash,
	}

	if !p.CreatedAt.IsZero() {
		query = `
			INSERT INTO payments (
				user_id, order_id, amount_minor, currency, status,
				idempotency_key, request_hash, created_at, updated_at
			) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?)`
		createdFormatted := p.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		updatedAt := p.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = p.CreatedAt
		}
		updatedFormatted := updatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		args = append(args, createdFormatted, updatedFormatted)
	}

	exec := getExecutor(ctx, r.db, nil)
	res, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		if classified := classifySQLiteError(err); classified != err {
			return classified
		}
		return fmt.Errorf("insert payment: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}
	p.ID = id
	return nil
}

func (r *PaymentRepository) UpdateSession(ctx context.Context, paymentID int64, providerPaymentID, checkoutURL string) error {
	return r.UpdateSessionTx(ctx, nil, paymentID, providerPaymentID, checkoutURL)
}

func (r *PaymentRepository) UpdateSessionTx(ctx context.Context, tx *sql.Tx, paymentID int64, providerPaymentID, checkoutURL string) error {
	exec := getExecutor(ctx, r.db, tx)
	query := `
			UPDATE payments
			SET provider_payment_id = ?, checkout_url = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND status = 'pending'`
	res, err := exec.ExecContext(ctx, query, providerPaymentID, checkoutURL, paymentID)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected rows: %w", err)
	}
	if affected == 0 {
		var status string
		checkErr := exec.QueryRowContext(ctx, "SELECT status FROM payments WHERE id = ?", paymentID).Scan(&status)
		if checkErr != nil {
			if errors.Is(checkErr, sql.ErrNoRows) {
				return domain.ErrPaymentNotFound
			}
			return fmt.Errorf("check payment existence: %w", checkErr)
		}
		return domain.ErrStatusConflict
	}
	return nil
}

func (r *PaymentRepository) UpdateStatus(ctx context.Context, paymentID int64, fromStatus, toStatus domain.PaymentStatus) error {
	return r.UpdateStatusTx(ctx, nil, paymentID, fromStatus, toStatus)
}

func (r *PaymentRepository) UpdateStatusTx(ctx context.Context, tx *sql.Tx, paymentID int64, fromStatus, toStatus domain.PaymentStatus) error {
	exec := getExecutor(ctx, r.db, tx)
	res, err := exec.ExecContext(ctx, `
			UPDATE payments
			SET status = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND status = ?`,
		string(toStatus), paymentID, string(fromStatus))
	if err != nil {
		if classified := classifySQLiteError(err); classified != err {
			return classified
		}
		return fmt.Errorf("update payment status: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected rows: %w", err)
	}
	if affected == 0 {
		var dummy int64
		checkErr := exec.QueryRowContext(ctx, "SELECT id FROM payments WHERE id = ?", paymentID).Scan(&dummy)
		if checkErr != nil {
			if errors.Is(checkErr, sql.ErrNoRows) {
				return domain.ErrPaymentNotFound
			}
			return fmt.Errorf("check payment existence: %w", checkErr)
		}
		return domain.ErrStatusConflict
	}
	return nil
}

type WebhookEventRepository struct {
	db *sql.DB
}

func NewWebhookEventRepository(db *sql.DB) *WebhookEventRepository {
	return &WebhookEventRepository{db: db}
}

func (r *WebhookEventRepository) RecordEvent(ctx context.Context, eventID, eventType string, payload []byte) (bool, error) {
	return r.RecordEventTx(ctx, nil, eventID, eventType, payload)
}

func (r *WebhookEventRepository) RecordEventTx(ctx context.Context, tx *sql.Tx, eventID, eventType string, payload []byte) (bool, error) {
	exec := getExecutor(ctx, r.db, tx)
	query := `
			INSERT INTO webhook_events (event_id, event_type, payload)
			VALUES (?, ?, ?)
			ON CONFLICT(event_id) DO NOTHING`

	res, err := exec.ExecContext(ctx, query, eventID, eventType, string(payload))
	if err != nil {
		return false, fmt.Errorf("insert webhook event: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check affected rows: %w", err)
	}
	if affected == 0 {
		return true, nil
	}
	return false, nil
}

type PaymentEventRepository struct {
	db *sql.DB
}

func NewPaymentEventRepository(db *sql.DB) *PaymentEventRepository {
	return &PaymentEventRepository{db: db}
}

func (r *PaymentEventRepository) RecordEvent(ctx context.Context, e domain.PaymentEvent) error {
	return r.RecordEventTx(ctx, nil, e)
}

func (r *PaymentEventRepository) RecordEventTx(ctx context.Context, tx *sql.Tx, e domain.PaymentEvent) error {
	exec := getExecutor(ctx, r.db, tx)
	query := `
			INSERT INTO payment_events (payment_id, event_type, from_status, to_status, metadata)
			VALUES (?, ?, ?, ?, ?)`
	_, err := exec.ExecContext(ctx, query, e.PaymentID, e.EventType, e.FromStatus, e.ToStatus, e.Metadata)
	if err != nil {
		return fmt.Errorf("insert payment event: %w", err)
	}
	return nil
}

func (r *PaymentEventRepository) ListByPaymentID(ctx context.Context, paymentID int64) ([]domain.PaymentEvent, error) {
	exec := getExecutor(ctx, r.db, nil)
	rows, err := exec.QueryContext(ctx, `
			SELECT id, payment_id, event_type, from_status, to_status, metadata, created_at
			FROM payment_events
			WHERE payment_id = ?
			ORDER BY id ASC`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("query payment events: %w", err)
	}
	defer rows.Close()

	var events []domain.PaymentEvent
	for rows.Next() {
		var e domain.PaymentEvent
		var fromStatus, toStatus, metadata sql.NullString
		var createdAt string
		if err := rows.Scan(&e.ID, &e.PaymentID, &e.EventType, &fromStatus, &toStatus, &metadata, &createdAt); err != nil {
			return nil, err
		}
		e.FromStatus = fromStatus.String
		e.ToStatus = toStatus.String
		e.Metadata = metadata.String
		parsedCreated, err := parseDBTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("scan payment event created_at: %w", err)
		}
		e.CreatedAt = parsedCreated
		events = append(events, e)
	}
	return events, rows.Err()
}

func (r *PaymentEventRepository) GetManualReviewEvents(ctx context.Context) ([]domain.PaymentEvent, error) {
	exec := getExecutor(ctx, r.db, nil)
	rows, err := exec.QueryContext(ctx, `
		SELECT id, payment_id, event_type, COALESCE(from_status, ''), COALESCE(to_status, ''), COALESCE(metadata, ''), created_at
		FROM payment_events
		WHERE event_type IN ('payment.late_success_requires_refund', 'payment.duplicate_requires_refund')
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query manual review events: %w", err)
	}
	defer rows.Close()

	var events []domain.PaymentEvent
	for rows.Next() {
		var e domain.PaymentEvent
		var fromS, toS string
		var createdAt string
		if err := rows.Scan(&e.ID, &e.PaymentID, &e.EventType, &fromS, &toS, &e.Metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("scan manual review event: %w", err)
		}
		e.FromStatus = fromS
		e.ToStatus = toS
		parsed, err := parseDBTime(createdAt)
		if err != nil {
			return nil, err
		}
		e.CreatedAt = parsed
		events = append(events, e)
	}
	return events, rows.Err()
}

type SecurityEventRepository struct {
	db *sql.DB
}

func NewSecurityEventRepository(db *sql.DB) *SecurityEventRepository {
	return &SecurityEventRepository{db: db}
}

func (r *SecurityEventRepository) RecordSecurityEvent(ctx context.Context, e domain.SecurityEvent) error {
	return r.RecordSecurityEventTx(ctx, nil, e)
}

func (r *SecurityEventRepository) RecordSecurityEventTx(ctx context.Context, tx *sql.Tx, e domain.SecurityEvent) error {
	exec := getExecutor(ctx, r.db, tx)
	query := `
			INSERT INTO security_events (payment_id, event_type, metadata)
			VALUES (?, ?, ?)`
	var paymentID sql.NullInt64
	if e.PaymentID != nil {
		paymentID = sql.NullInt64{Int64: *e.PaymentID, Valid: true}
	}
	_, err := exec.ExecContext(ctx, query, paymentID, e.EventType, e.Metadata)
	if err != nil {
		return fmt.Errorf("insert security event: %w", err)
	}
	return nil
}

func (r *SecurityEventRepository) ListSecurityEvents(ctx context.Context) ([]domain.SecurityEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, payment_id, event_type, COALESCE(metadata, ''), created_at
		FROM security_events
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query security events: %w", err)
	}
	defer rows.Close()

	var events []domain.SecurityEvent
	for rows.Next() {
		var e domain.SecurityEvent
		var pid sql.NullInt64
		var createdAt string
		if err := rows.Scan(&e.ID, &pid, &e.EventType, &e.Metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("scan security event: %w", err)
		}
		if pid.Valid {
			e.PaymentID = &pid.Int64
		}
		parsed, err := parseDBTime(createdAt)
		if err != nil {
			return nil, err
		}
		e.CreatedAt = parsed
		events = append(events, e)
	}
	return events, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrder(row rowScanner) (*domain.Order, error) {
	var o domain.Order
	var status string
	var createdAt, updatedAt string
	err := row.Scan(&o.ID, &o.UserID, &o.AmountMinor, &o.Currency, &status, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrOrderNotFound
		}
		return nil, fmt.Errorf("scan order: %w", err)
	}

	o.Status = domain.OrderStatus(status)
	parsedCreated, err := parseDBTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("scan order created_at: %w", err)
	}
	o.CreatedAt = parsedCreated
	parsedUpdated, err := parseDBTime(updatedAt)
	if err != nil {
		return nil, fmt.Errorf("scan order updated_at: %w", err)
	}
	o.UpdatedAt = parsedUpdated
	return &o, nil
}

func scanPayment(row rowScanner) (*domain.Payment, error) {
	var p domain.Payment
	var status string
	var providerID, checkoutURL sql.NullString
	var createdAt, updatedAt string

	err := row.Scan(
		&p.ID, &p.UserID, &p.OrderID, &p.AmountMinor, &p.Currency, &status,
		&p.IdempotencyKey, &p.RequestHash, &providerID, &checkoutURL,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrPaymentNotFound
		}
		return nil, fmt.Errorf("scan payment: %w", err)
	}

	p.Status = domain.PaymentStatus(status)
	if providerID.Valid {
		p.ProviderPaymentID = providerID.String
	}
	if checkoutURL.Valid {
		p.CheckoutURL = checkoutURL.String
	}
	parsedCreated, err := parseDBTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("scan payment created_at: %w", err)
	}
	p.CreatedAt = parsedCreated
	parsedUpdated, err := parseDBTime(updatedAt)
	if err != nil {
		return nil, fmt.Errorf("scan payment updated_at: %w", err)
	}
	p.UpdatedAt = parsedUpdated

	return &p, nil
}

func parseDBTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse db time %q", s)
}
