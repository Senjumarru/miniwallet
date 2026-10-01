package provider

import (
	"bytes"
	"context"
	crypto_rand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/metrics"
)

var (
	ErrProviderUnavailable = errors.New("payment provider unavailable")
	ErrProviderClientError = errors.New("payment provider rejected request (client error)")
	ErrProviderFatal       = errors.New("fatal provider error")

	// Однозначный отказ (4xx кроме 429)
	ErrDefinitiveRejection = errors.New("provider definitive rejection (4xx)")
	// Неоднозначный исход (таймаут, 5xx после повторов, сетевой разрыв)
	ErrAmbiguousOutcome = errors.New("provider ambiguous outcome (timeout or 5xx)")
)

// IsDefinitiveRejection проверяет, является ли отказ провайдера окончательным (4xx кроме 429).
func IsDefinitiveRejection(err error) bool {
	return errors.Is(err, ErrDefinitiveRejection) || errors.Is(err, ErrProviderClientError)
}

// IsAmbiguousOutcome проверяет, является ли исход вызова неоднозначным (таймаут, 5xx, разрыв).
func IsAmbiguousOutcome(err error) bool {
	return errors.Is(err, ErrAmbiguousOutcome) || errors.Is(err, ErrProviderUnavailable)
}

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Client struct {
	httpClient        HTTPClient
	baseURL           string
	logger            *slog.Logger
	timeout           time.Duration
	maxAttempts       int
	initialRetryDelay time.Duration
	maxRetryDelay     time.Duration
	sem               *Semaphore
	cb                *CircuitBreaker
	metrics           *metrics.Metrics
}

func NewClient(
	baseURL string,
	httpClient HTTPClient,
	logger *slog.Logger,
	timeout time.Duration,
	maxAttempts int,
	initialRetryDelay time.Duration,
	maxRetryDelay time.Duration,
) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if maxAttempts <= 0 {
		maxAttempts = 4
	}
	if initialRetryDelay <= 0 {
		initialRetryDelay = 100 * time.Millisecond
	}
	if maxRetryDelay <= 0 {
		maxRetryDelay = 2 * time.Second
	}
	return &Client{
		httpClient:        httpClient,
		baseURL:           baseURL,
		logger:            logger,
		timeout:           timeout,
		maxAttempts:       maxAttempts,
		initialRetryDelay: initialRetryDelay,
		maxRetryDelay:     maxRetryDelay,
		sem:               NewSemaphore(20),
		cb:                NewCircuitBreaker(5, 5*time.Second, 1, domain.RealClock{}),
	}
}

func (c *Client) SetSemaphore(sem *Semaphore) {
	c.sem = sem
}

func (c *Client) SetCircuitBreaker(cb *CircuitBreaker) {
	c.cb = cb
}

func (c *Client) SetMetrics(m *metrics.Metrics) {
	c.metrics = m
}

func (c *Client) CircuitBreaker() *CircuitBreaker {
	return c.cb
}

func (c *Client) Semaphore() *Semaphore {
	return c.sem
}

// TODO(docs): Формат тела запроса инициализации сессии провайдера.
type providerCheckoutRequest struct {
	PaymentID   int64  `json:"payment_id"`
	OrderID     int64  `json:"order_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// TODO(docs): Формат тела ответа провайдера.
type providerCheckoutResponse struct {
	ProviderPaymentID string `json:"provider_payment_id"`
	CheckoutURL       string `json:"checkout_url"`
	Error             string `json:"error,omitempty"`
}

func (c *Client) CreateCheckoutSession(ctx context.Context, in CreateCheckoutInput) (CheckoutSession, error) {
	start := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.ProviderRequestDuration.Observe(time.Since(start).Seconds())
			if c.cb != nil {
				c.metrics.CircuitBreakerState.Set(float64(c.cb.State()))
			}
		}
	}()

	// 5.4: Семафор параллельных вызовов провайдера
	if c.sem != nil {
		if err := c.sem.Acquire(ctx); err != nil {
			return CheckoutSession{}, err
		}
		defer c.sem.Release()
	}

	// 5.4: Circuit breaker проверка перед сетевым запросом
	if c.cb != nil {
		if err := c.cb.Allow(); err != nil {
			return CheckoutSession{}, err
		}
	}

	reqBody, err := json.Marshal(providerCheckoutRequest{
		PaymentID:   in.PaymentID,
		OrderID:     in.OrderID,
		AmountMinor: in.AmountMinor,
		Currency:    in.Currency,
	})
	if err != nil {
		return CheckoutSession{}, fmt.Errorf("marshal provider request: %w", err)
	}

	url := c.baseURL + "/v1/checkout/sessions"

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return CheckoutSession{}, fmt.Errorf("context canceled before attempt %d: %w", attempt, err)
		}

		c.logger.InfoContext(ctx, "provider call attempt",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", c.maxAttempts),
			slog.Int64("payment_id", in.PaymentID),
			slog.Int64("order_id", in.OrderID),
		)

		session, retryable, waitDuration, err := c.doAttempt(ctx, url, reqBody, in.IdempotencyKey)
		if err == nil {
			if c.cb != nil {
				c.cb.OnSuccess()
			}
			return session, nil
		}

		lastErr = err

		c.logger.WarnContext(ctx, "provider call failed",
			slog.Int("attempt", attempt),
			slog.Int64("payment_id", in.PaymentID),
			slog.Int64("order_id", in.OrderID),
			slog.Bool("retryable", retryable),
			slog.String("error", err.Error()),
		)

		if !retryable {
			return CheckoutSession{}, lastErr
		}
		if attempt == c.maxAttempts {
			break
		}

		if c.metrics != nil {
			c.metrics.ProviderRetriesTotal.Inc()
		}

		backoff := c.calculateBackoff(attempt, waitDuration)

		// 5.4: Бюджет повторов (не повторять, если дедлайн запроса почти исчерпан)
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			minBudget := backoff + c.timeout
			if minBudget < c.initialRetryDelay {
				minBudget = c.initialRetryDelay
			}
			if remaining <= backoff || remaining < c.timeout || remaining < minBudget {
				c.logger.WarnContext(ctx, "retry budget exhausted: deadline almost expired",
					slog.Duration("remaining", remaining),
					slog.Duration("required_backoff", backoff),
					slog.Duration("min_budget", minBudget),
					slog.Duration("timeout", c.timeout),
				)
				break
			}
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return CheckoutSession{}, fmt.Errorf("context canceled during retry backoff: %w", ctx.Err())
		case <-timer.C:
		}
	}

	if c.cb != nil {
		c.cb.OnFailure()
	}

	return CheckoutSession{}, fmt.Errorf("%w: %w", ErrAmbiguousOutcome, lastErr)
}

func (c *Client) doAttempt(ctx context.Context, url string, body []byte, idempotencyKey string) (CheckoutSession, bool, time.Duration, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return CheckoutSession{}, false, 0, fmt.Errorf("create http request: %w", err)
	}

	// TODO(docs): Имя заголовка идемпотентности провайдера.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Сетевые ошибки и таймауты подлежат retry.
		return CheckoutSession{}, isNetworkOrTimeout(err), 0, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return CheckoutSession{}, true, 0, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out providerCheckoutResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			return CheckoutSession{}, false, 0, fmt.Errorf("unmarshal success response: %w", err)
		}
		if out.ProviderPaymentID == "" || out.CheckoutURL == "" {
			return CheckoutSession{}, false, 0, fmt.Errorf("invalid provider response: missing required fields")
		}
		return CheckoutSession{
			ProviderPaymentID: out.ProviderPaymentID,
			CheckoutURL:       out.CheckoutURL,
		}, false, 0, nil
	}

	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	// 429 Too Many Requests: подлежит повтору с учетом Retry-After
	if resp.StatusCode == http.StatusTooManyRequests {
		return CheckoutSession{}, true, retryAfter, fmt.Errorf("rate limited (429)")
	}

	// 5xx ошибки сервера провайдера: подлежат retry с учетом Retry-After
	if resp.StatusCode >= 500 {
		return CheckoutSession{}, true, retryAfter, fmt.Errorf("provider 5xx error: status %d", resp.StatusCode)
	}

	// 4xx ошибки клиента (кроме 429): НЕ повторяем
	return CheckoutSession{}, false, 0, fmt.Errorf("%w: %w: status %d", ErrDefinitiveRejection, ErrProviderClientError, resp.StatusCode)
}

func (c *Client) GetPaymentStatus(ctx context.Context, providerPaymentID string) (domain.PaymentStatus, error) {
	if c.sem != nil {
		if err := c.sem.Acquire(ctx); err != nil {
			return "", err
		}
		defer c.sem.Release()
	}

	if c.cb != nil {
		if err := c.cb.Allow(); err != nil {
			return "", err
		}
	}

	url := fmt.Sprintf("%s/v1/payments/%s", c.baseURL, providerPaymentID)

	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create status request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if c.cb != nil {
			c.cb.OnFailure()
		}
		return "", fmt.Errorf("network error on status check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		if c.cb != nil {
			c.cb.OnSuccess()
		}
		return domain.PaymentStatusFailed, nil
	}
	if resp.StatusCode != http.StatusOK {
		if c.cb != nil {
			c.cb.OnFailure()
		}
		return "", fmt.Errorf("provider returned status %d on check", resp.StatusCode)
	}

	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode provider status: %w", err)
	}

	if c.cb != nil {
		c.cb.OnSuccess()
	}

	switch out.Status {
	case "succeeded":
		return domain.PaymentStatusSucceeded, nil
	case "failed":
		return domain.PaymentStatusFailed, nil
	case "canceled":
		return domain.PaymentStatusCanceled, nil
	default:
		return domain.PaymentStatusPending, nil
	}
}

func (c *Client) calculateBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	// Full Jitter: Sleep = rand(0, min(maxDelay, baseDelay * 2^(attempt-1)))
	multiplier := 1 << (attempt - 1)
	capDelay := c.initialRetryDelay * time.Duration(multiplier)
	if capDelay > c.maxRetryDelay {
		capDelay = c.maxRetryDelay
	}
	if capDelay <= 0 {
		return 0
	}
	n, err := crypto_rand.Int(crypto_rand.Reader, big.NewInt(int64(capDelay)))
	if err != nil {
		return capDelay / 2
	}
	return time.Duration(n.Int64())
}

func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if targetTime, err := http.ParseTime(val); err == nil {
		diff := time.Until(targetTime)
		if diff > 0 {
			return diff
		}
	}
	return 0
}

func isNetworkOrTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}
