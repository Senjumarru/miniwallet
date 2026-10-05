# Заметки аудита архитектуры и технического долга (AUDIT_NOTES.md)

В данном документе зафиксированы архитектурные компромиссы, выявленный технический долг и особенности реализации подсистем `miniwallet`.

---

## 1. Архитектурный долг: Импорт `database/sql` в слое бизнес-логики (`internal/service`)

### Описание
Согласно принципам Clean Architecture и DDD, слой бизнес-логики (`internal/service`) должен зависеть исключительно от доменных сущностей (`internal/domain`) и абстрактных интерфейсов, не привязываясь к конкретным инфраструктурным библиотекам.

В текущей реализации пакет `internal/service` напрямую импортирует `database/sql` в следующих файлах:
- `internal/service/interfaces.go` — сигнатуры методов `GetByIDTx`, `UpdateStatusTx`, `RecordEventTx`, `RecordSecurityEventTx` и `WithinTransaction` объявляют параметр `tx *sql.Tx`.
- `internal/service/payment.go` — вызовы транзакционного менеджера `s.txManager.WithinTransaction(ctx, func(txCtx context.Context, tx *sql.Tx) error { ... })` передают `tx` в методы репозиториев.
- `internal/service/reconciler.go` — транзакционная сверка зависших платежей также оперирует `tx *sql.Tx`.

### Статус и проверка
В архитектурном наборе тестов `internal/arch/arch_test.go` добавлен тест:
```go
func TestServiceDoesNotImportDatabaseSQL(t *testing.T) {
    t.Skip("known debt: internal/service imports database/sql in interfaces.go, payment.go, reconciler.go for TxManager and *sql.Tx")
    ...
}
```
Тест помечен `t.Skip` с причиной `"known debt"` во избежание блокировки пайплайна.

### Рекомендации по устранению долга
1. **Tx-in-Context паттерн**: Передавать транзакционный контекст не через явный указатель `tx *sql.Tx`, а через контекст `context.Context` (например, `ctx = contextWithTx(ctx, tx)`). Репозитории на уровне `internal/storage/sqlite` извлекают `*sql.Tx` из `ctx`, а `internal/service` оперирует только стандартным `context.Context`.
2. **Абстрактный интерфейс транзакции**: Заменить `*sql.Tx` на доменный интерфейс транзакции `domain.Tx` / `service.Tx`, реализуемый инфраструктурными адаптерами.

---

## 2. Механизм транзакций и отката в `internal/storage/memory`

### Реализация
В `internal/storage/memory/memory.go` транзакционный менеджер `WithinTransaction` реализован на основе паттерна **Deep Snapshot / Restore**:

```go
func (s *Storage) WithinTransaction(ctx context.Context, fn func(txCtx context.Context, tx *sql.Tx) error) error {
    s.txMu.Lock()
    defer s.txMu.Unlock()

    s.mu.Lock()
    snap := s.snapshot()
    s.mu.Unlock()

    if err := fn(ctx, nil); err != nil {
        s.mu.Lock()
        s.restore(snap)
        s.mu.Unlock()
        return err
    }
    return nil
}
```

### Как имитируется откат
1. **Сериализация транзакций**: Мьютекс `s.txMu.Lock()` блокирует параллельные транзакции на время выполнения `fn`, имитируя поведение SQLite `BEGIN IMMEDIATE` / `EXCLUSIVE`.
2. **Создание снимка (Snapshot)**: Метод `s.snapshot()` под блокировкой `s.mu.Lock()` создает глубокую копию состояния хранилища:
   - Всех мап: `users`, `orders`, `payments`, `paymentEvents`, `webhookEvts`, `userKeyIndex`.
   - Среза `securityEvts`.
   - Автоинкрементных счетчиков `nextPayID`, `nextEventID`.
3. **Выполнение**: Замыкание `fn` выполняется над текущим состоянием хранилища.
4. **Откат при ошибке (Rollback)**: Если `fn` возвращает любую ошибку (`err != nil`), вызывается `s.restore(snap)`, который атомарно перезаписывает все мапы и счетчики сохранённым снимком `snap`.
5. **Фиксация (Commit)**: Если `fn` возвращает `nil`, снимок освобождается, и все произведенные изменения остаются в силе.

### Ограничения и особенности имитации
- **Параметр `tx == nil`**: In-memory хранилище не имеет реального SQL-движка, поэтому передает в колбэк `tx = nil`.
- **Игнорирование `tx` в методах `...Tx`**: Репозитории `UserRepo`, `OrderRepo`, `PaymentRepo`, `WebhookRepo`, `PaymentEventRepo`, `SecurityEventRepo` принимают аргумент `tx *sql.Tx`, но не вызывают на нем SQL-методы (`tx.Exec`, `tx.Query`), оперируя структурами Go. Прямой вызов методов на `tx` внутри `fn` привёл бы к `nil pointer dereference`.
- **Верификация**: Полноценный откат подтвержден контрактными тестами `TestContract_ProcessWebhook_AtomicityRollback`, где сбой на шаге обновления заказа подтверждает полный возврат статусов платежа и отсутствие частичных изменений в `memory.Storage`.
