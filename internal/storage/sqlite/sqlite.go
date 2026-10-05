package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/Senjumarru/miniwallet/internal/domain"
	_ "modernc.org/sqlite"
)

type Storage struct {
	db *sql.DB
}

func Open(dbPath, migrationsDir string) (*Storage, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_txlock=immediate", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(50)
	db.SetConnMaxLifetime(0)

	s := &Storage{db: db}
	if migrationsDir != "" {
		if err := s.migrate(migrationsDir); err != nil {
			if closeErr := db.Close(); closeErr != nil {
				return nil, errors.Join(fmt.Errorf("apply migrations: %w", err), fmt.Errorf("close db: %w", closeErr))
			}
			return nil, fmt.Errorf("apply migrations: %w", err)
		}
	}

	return s, nil
}

func (s *Storage) DB() *sql.DB {
	return s.db
}

func (s *Storage) Close() error {
	return s.db.Close()
}

func (s *Storage) Users() *UserRepository                   { return NewUserRepository(s.db) }
func (s *Storage) Orders() *OrderRepository                 { return NewOrderRepository(s.db) }
func (s *Storage) Payments() *PaymentRepository             { return NewPaymentRepository(s.db) }
func (s *Storage) Webhooks() *WebhookEventRepository        { return NewWebhookEventRepository(s.db) }
func (s *Storage) PaymentEvents() *PaymentEventRepository   { return NewPaymentEventRepository(s.db) }
func (s *Storage) SecurityEvents() *SecurityEventRepository { return NewSecurityEventRepository(s.db) }

func (s *Storage) SeedUser(u *domain.User) {
	isBlocked := 0
	if u.IsBlocked {
		isBlocked = 1
	}
	isActive := 1
	if !u.IsActive {
		isActive = 0
	}
	_, err := s.db.Exec(`
		INSERT INTO users (id, email, is_active, is_blocked)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			email = excluded.email,
			is_active = excluded.is_active,
			is_blocked = excluded.is_blocked`,
		u.ID, u.Email, isActive, isBlocked)
	if err != nil {
		slog.Error("seed user failed", slog.String("error", err.Error()), slog.Int64("user_id", u.ID))
	}
}

func (s *Storage) SeedOrder(o *domain.Order) {
	status := string(o.Status)
	if status == "" {
		status = "unpaid"
	}
	curr := o.Currency
	if curr == "" {
		curr = "KZT"
	}
	_, err := s.db.Exec(`
		INSERT INTO orders (id, user_id, amount_minor, currency, status)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user_id = excluded.user_id,
			amount_minor = excluded.amount_minor,
			currency = excluded.currency,
			status = excluded.status`,
		o.ID, o.UserID, o.AmountMinor, curr, status)
	if err != nil {
		slog.Error("seed order failed", slog.String("error", err.Error()), slog.Int64("order_id", o.ID))
	}
}

type txContextKey struct{}

// ContextWithTx injects an active SQL transaction into context.
func ContextWithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// TxFromContext extracts an active SQL transaction from context, or returns nil.
func TxFromContext(ctx context.Context) *sql.Tx {
	if ctx == nil {
		return nil
	}
	if tx, ok := ctx.Value(txContextKey{}).(*sql.Tx); ok {
		return tx
	}
	return nil
}

func (s *Storage) WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	txCtx := ContextWithTx(ctx, tx)
	if err := fn(txCtx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback transaction: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *Storage) migrate(migrationsDir string) error {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		cleanPath := filepath.Clean(filepath.Join(migrationsDir, name))
		data, err := os.ReadFile(cleanPath)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := s.db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}
