# Заметки аудита архитектуры и технического долга (AUDIT_NOTES.md)

В данном документе зафиксированы архитектурные компромиссы, выявленный технический долг и особенности реализации подсистем `miniwallet`.

---

## 1. Архитектурный долг: Импорт `database/sql` в слое бизнес-логики (`internal/service`) [УСТРАНЕНО]

### Описание
Согласно принципам Clean Architecture и DDD, слой бизнес-логики (`internal/service`) должен зависеть исключительно от доменных сущностей (`internal/domain`) и абстрактных интерфейсов, не привязываясь к конкретным инфраструктурным библиотекам.

Ранее пакет `internal/service` импортировал `database/sql` в `interfaces.go`, `payment.go` и `reconciler.go` для передачи `tx *sql.Tx`.

### Статус и проверка
Долг полностью устранён внедрением паттерна **Tx-in-Context**:
1. Транзакционный менеджер `WithinTransaction(ctx, func(txCtx context.Context) error)` инжектирует активную транзакцию в контекст (`sqlite.ContextWithTx(ctx, tx)`).
2. Репозитории `sqlite` извлекают транзакцию из контекста (`TxFromContext(ctx)`) через `dbExecutor`, прозрачно используя транзакцию или пул подключений `*sql.DB`.
3. In-memory хранилище `memory` работает аналогично через снимок и откат без каких-либо ссылок на `sql.Tx`.
4. В `internal/arch/arch_test.go` снят `t.Skip` с теста `TestServiceDoesNotImportDatabaseSQL`, и тест успешно проходит (`PASS`).
5. Пакет `internal/service` больше не импортирует `database/sql` ни в одном файле.

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
