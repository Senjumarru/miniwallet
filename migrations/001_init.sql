-- 001_init.sql — Схема платёжного сервиса

PRAGMA foreign_keys = ON;

-- 1. Пользователи
CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    email      TEXT    NOT NULL UNIQUE,
    is_active  INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
    is_blocked INTEGER NOT NULL DEFAULT 0 CHECK (is_blocked IN (0, 1)),
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- 2. Заказы (деньги в тиынах, валюта строго KZT)
CREATE TABLE IF NOT EXISTS orders (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id),
    amount_minor INTEGER NOT NULL CHECK (amount_minor > 0),
    currency     TEXT    NOT NULL DEFAULT 'KZT' CHECK (currency = 'KZT'),
    status       TEXT    NOT NULL DEFAULT 'unpaid' CHECK (status IN ('unpaid', 'paid', 'canceled')),
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- 3. Платежи
CREATE TABLE IF NOT EXISTS payments (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id             INTEGER NOT NULL REFERENCES users(id),
    order_id            INTEGER NOT NULL REFERENCES orders(id),
    amount_minor        INTEGER NOT NULL CHECK (amount_minor > 0),
    currency            TEXT    NOT NULL CHECK (currency = 'KZT'),
    status              TEXT    NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'canceled')),
    idempotency_key     TEXT    NOT NULL,
    request_hash        TEXT    NOT NULL,
    provider_payment_id TEXT,
    checkout_url        TEXT,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CONSTRAINT uq_user_idempotency UNIQUE (user_id, idempotency_key)
);

-- Частичный уникальный индекс: на один заказ НЕ МОЖЕТ быть двух активных pending-платежей
CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_order_single_pending
    ON payments (order_id)
    WHERE status = 'pending';

-- Частичный уникальный индекс: на один заказ НЕ МОЖЕТ быть двух успешных платежей
CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_order_single_succeeded
    ON payments (order_id)
    WHERE status = 'succeeded';

-- Индекс для фоновой сверки pending-платежей по TTL
CREATE INDEX IF NOT EXISTS idx_payments_pending_created
    ON payments (status, created_at)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_payments_provider_id
    ON payments (provider_payment_id);

-- 4. Дедупликация событий вебхуков (idempotency провайдера)
CREATE TABLE IF NOT EXISTS webhook_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT    NOT NULL UNIQUE,
    event_type   TEXT    NOT NULL,
    payload      TEXT    NOT NULL,
    processed_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- 5. Журнал аудита состояний платежей (payment_events)
CREATE TABLE IF NOT EXISTS payment_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    payment_id  INTEGER NOT NULL REFERENCES payments(id),
    event_type  TEXT    NOT NULL,
    from_status TEXT,
    to_status   TEXT,
    metadata    TEXT, -- JSON с контекстом события
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_payment_events_payment_id
    ON payment_events (payment_id);

-- 6. Журнал событий безопасности (security_events) для событий без обязательной привязки к платежу
CREATE TABLE IF NOT EXISTS security_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    payment_id INTEGER REFERENCES payments(id), -- NULLable
    event_type TEXT    NOT NULL,
    metadata   TEXT, -- JSON с контекстом события
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_security_events_created
    ON security_events (created_at);

