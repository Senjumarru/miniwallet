# Чек-лист безопасности miniwallet (OWASP API Security Top 10)

В данном документе зафиксировано соответствие платёжного сервиса `miniwallet` требованиям стандарта **OWASP API Security Top 10 (2023)**.

---

| Категория OWASP API Security | Статус | Архитектурная реализация в проекте |
| :--- | :---: | :--- |
| **API1:2023 Broken Object Level Authorization (BOLA)** | **PASS** | Проверка владения заказом: `order.UserID != in.UserID` возвращает единую ошибку `ErrOrderNotFound` (404), предотвращая горизонтальное повышение привилегий и enumeration чужих сущностей. |
| **API2:2023 Broken Authentication** | **PASS** | Идентификация клиента осуществляется строго через валидацию JWT токена в `JWTMiddleware` (HMAC-SHA256). Заголовок `X-User-ID` и тело `user_id` не принимаются. Вебхуки защищены HMAC-SHA256 подписью по сырому телу с ротацией ключей и проверкой таймстемпа (±5 мин). |
| **API3:2023 Broken Object Property Level Authorization** | **PASS** | Сумма (`AmountMinor`) и валюта (`Currency`) берутся исключительно из записи заказа в БД. Параметры вебхука валидируются против сохранённого платежа; клиент не может подменить сумму платежа через тело запроса. |
| **API4:2023 Unrestricted Resource Consumption** | **PASS** | Многоуровневый Rate Limiting: по IP (`100 rps, burst 200`), по User ID (`20 rps, burst 40`), по Webhook IP (`50 rps, burst 100`). HTTP сервер защищён таймаутами чтения/записи и ограничением `MaxHeaderBytes` (1MB). Сетевые вызовы к PSP ограничены семафором на 20 соединений. |
| **API5:2023 Broken Function Level Authorization** | **PASS** | Административные или критические операции изолированы. Проверка активности и блокировки пользователя (`is_active`, `is_blocked`) перед созданием транзакции. |
| **API6:2023 Unrestricted Access to Sensitive Business Flows** | **PASS** | Защита от дублирования списаний и гонок: уникальный составной индекс `(user_id, idempotency_key)`, атомарная вставка pending-платежа, частичный индекс `payments(order_id) WHERE status='succeeded'`, запрещающий повторную оплату одного заказа. |
| **API7:2023 Server Side Request Forgery (SSRF)** | **PASS** | URL шлюза провайдера задаётся исключительно через конфигурацию окружения (`PROVIDER_BASE_URL`). Никакие внешние URL из тел запросов клиентов не используются для исходящих сетевых вызовов. |
| **API8:2023 Security Misconfiguration** | **PASS** | Унифицированные ответы об ошибках без раскрытия стектрейсов и деталей SQLite (`writeDomainError`). SQLite открывается с `foreign_keys=ON`, `WAL`, `busy_timeout=5000` и `_txlock=immediate`. Права на директорию БД ограничены `0750`. |
| **API9:2023 Improper Inventory Management** | **PASS** | Чёткое версионирование маршрутов: `POST /payments`, `POST /webhooks/provider`, эндпоинты жизнеспособности `/healthz`, `/readyz`, `/metrics`. Документированные контракты в `requests.http` и коде. |
| **API10:2023 Unsafe Consumption of APIs** | **PASS** | Защита при взаимодействии со сторонним провайдером (PSP): Circuit Breaker с переходом в Open при сбоях (быстрый отказ 503), валидация ответов, Retry Budget с экспоненциальным full jitter, обработка разрывов сети без перевода платежа в `failed` (защита от ложного списания). |

---

## Автоматизированные проверки безопасности в CI
* **govulncheck**: проверка зависимостей на уязвимости (0 CVE).
* **gosec**: статический анализ безопасности исходного кода Go (0 замечаний).
* **gitleaks**: сканирование репозитория на утечку секретов и API-ключей (0 утечек).
* **go test -race**: полное отсутствие race conditions на 200 параллельных итерациях.
