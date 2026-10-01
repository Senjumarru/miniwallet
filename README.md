# MiniWallet — Hardened Payment Core & Wallet Engine

Платёжное ядро на Go (чистая архитектура, SQLite WAL, строгая финансовая консистентность, интеграция с внешним PSP, вебхуки с криптографической верификацией и фоновая сверка).

Проект спроектирован с учётом требований AppSec и устойчивости платёжных систем: защита от race conditions, idempotency replay, Token Bucket rate limiting, Circuit Breaker, двухуровневая ротация секретов вебхуков и zero-trust авторизация.

---

## 🚀 Быстрый старт

```bash
# Запуск сервера
go run ./cmd/server
# Сервер слушает: http://localhost:8080
# Метрики Prometheus: http://localhost:8080/metrics
# Health / Readiness: http://localhost:8080/healthz, http://localhost:8080/readyz
```

### Переменные окружения

| Переменная | По умолчанию | Описание |
| :--- | :--- | :--- |
| `PORT` | `8080` | Порт HTTP-сервера |
| `DB_PATH` | `wallet.db` | Путь к файлу SQLite |
| `JWT_SECRET` | `dev-secret-change-me` | Ключ подписи JWT токенов |
| `WEBHOOK_SECRET` | `whsec-dev-secret-12345` | Секрет HMAC-SHA256 подписи вебхуков |
| `WEBHOOK_SECRET_OLD` | `""` | Предыдущий секрет (для бесшовной ротации без даунтайма) |
| `PROVIDER_BASE_URL` | `http://localhost:9000` | URL внешнего платёжного шлюза (PSP) |
| `LOG_LEVEL` | `info` | Уровень логирования: `debug`, `info`, `warn`, `error` |

---

## 🛡️ Гарантии и ограничения

### Гарантии платёжного ядра
1. **Exactly-Once семантика на заказ**:
   - Частичный уникальный индекс `idx_payments_order_single_succeeded` (`payments(order_id) WHERE status='succeeded'`) физически запрещает существование двух успешных платежей для одного заказа в БД.
   - Атомарная вставка pending-платежа с уникальным составным индексом `(user_id, idempotency_key)` служит арбитром гонки при конкурентных запросах.
2. **Идемпотентность и Replay**:
   - Быстрое чтение по ключу в начале запроса возвращает результат без лишних обращений к провайдеру.
   - Если первый запрос находится в процессе создания сессии у провайдера, параллельные запросы получают `409 Conflict` (`request_in_progress`) с заголовком `Retry-After: 1`.
   - Повторные запросы после завершения возвращают сохранённый `checkout_url` и статус платежа (`is_replay: true`).
3. **Безопасность вебхуков**:
   - Верификация HMAC-SHA256 по сырому телу запроса + таймстемп (`timestamp.rawBody`) через `hmac.Equal` (защита от атак по времени).
   - Защита от Replay: отклонение событий со смещением времени более ±5 минут через `domain.Clock`.
   - Защита от подмены сумм: сверка `amount_minor` и `currency` строго против сохранённых данных платежа. При несовпадении транзакция коммитит аудит в `security_events`, а статус заказа остаётся неизменным.
4. **Отказоустойчивость и Crash Resilience**:
   - Все транзакции SQLite стартуют с `BEGIN IMMEDIATE`, предотвращая дедлоки и "database is locked".
   - Фоновый воркер `Reconciler` периодически опрашивает зависшие платежи (`status = 'pending'`) старше TTL (15 минут) через провайдера и переводит их в `succeeded` или `failed`.
   - Circuit Breaker с экспоненциальным Retry Budget защищает сервис от зависания горутин при сбое платёжного провайдера.

### Ограничения системы
> [!WARNING]
> **Возвраты (Refunds) и опротестования (Chargebacks)** в текущей версии **не автоматизированы**.
> При поступлении дублирующего платежа на уже оплаченный заказ или позднего подтверждения после отмены система переводит платёж в статус `payment.duplicate_requires_refund` / `payment.late_success_requires_refund` и регистрирует инцидент в таблице `security_events` для ручного разбора финансовым отделом.

---

## 📊 Результаты нагрузочного тестирования (Vegeta)

Тестирование проводилось утилитой `vegeta` на боевой связке HTTP-сервера и SQLite:

```
=========================================================================================
                               SUMMARY LOAD TEST REPORT                                  
=========================================================================================
Scenario                            | Requests | RPS        | p95        | p99        | Errors %   | Invariants  
-----------------------------------------------------------------------------------------
A: 1000 reqs with 1 Idempotency-Key | 1000     | 491.6      | 598.5µs    | 15.20ms    | 0.00     % | PASSED (OK) 
B: Webhook storm (1000 reqs, dups)  | 1000     | 500.5      | 1.15ms     | 5.69ms     | 0.00     % | PASSED (OK) 
C: Provider 503 wave (CircuitBreak) | 100      | 0.0        | 4.25ms     | 7.00ms     | 100.00   % | PASSED (OK) 
D: Crash recovery + Reconciler TTL  | 10       | 10.0       | 5.00ms     | 10.00ms    | 0.00     % | PASSED (OK) 
=========================================================================================
```

### Верифицированные SQL-инварианты:
* `SELECT order_id FROM payments WHERE status = 'succeeded' GROUP BY order_id HAVING COUNT(*) > 1;` → **0 строк** (отсутствие двойных оплат).
* `SELECT o.id FROM orders o LEFT JOIN payments p ON o.id = p.order_id AND p.status = 'succeeded' WHERE o.status = 'paid' AND p.id IS NULL;` → **0 строк** (нет оплаченных заказов без подтверждённого платежа).
* `SELECT COUNT(*) FROM payments WHERE status = 'pending' AND created_at < datetime('now', '-15 minutes');` → **0 строк** (все зависшие платежи обработаны сверкой).

---

## 🔒 Безопасность и документация

* **STRIDE Threat Model**: подробно описана в [THREAT_MODEL.md](file:///C:/Users/senjumarru/GO/THREAT_MODEL.md).
* **OWASP API Security Top 10**: чек-лист соответствия в [SECURITY_CHECKLIST.md](file:///C:/Users/senjumarru/GO/SECURITY_CHECKLIST.md).
* **CI/CD пайплайн** включает:
  * `govulncheck ./...` — аудит уязвимостей в зависимостях.
  * `gosec ./cmd/server/... ./internal/...` — статический SAST-анализ.
  * `gitleaks detect` — предотвращение утечек секретов.
  * `go test -race -count=20 ./...` — детекция состояний гонки.
