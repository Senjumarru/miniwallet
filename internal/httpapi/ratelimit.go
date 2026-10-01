package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

type tokenBucket struct {
	rate       float64 // tokens per second
	burst      int
	tokens     float64
	lastUpdate time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64
	burst   int
	clock   domain.Clock
}

func NewRateLimiter(rate float64, burst int, clock domain.Clock) *RateLimiter {
	if rate <= 0 {
		rate = 10
	}
	if burst <= 0 {
		burst = 20
	}
	if clock == nil {
		clock = domain.RealClock{}
	}
	return &RateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    rate,
		burst:   burst,
		clock:   clock,
	}
}

func (rl *RateLimiter) SetClock(clock domain.Clock) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.clock = clock
}

func (rl *RateLimiter) Allow(key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now().UTC()
	b, exists := rl.buckets[key]
	if !exists {
		b = &tokenBucket{
			rate:       rl.rate,
			burst:      rl.burst,
			tokens:     float64(rl.burst) - 1.0,
			lastUpdate: now,
		}
		rl.buckets[key] = b
		return true, 0
	}

	elapsed := now.Sub(b.lastUpdate).Seconds()
	b.lastUpdate = now
	b.tokens += elapsed * b.rate
	if b.tokens > float64(b.burst) {
		b.tokens = float64(b.burst)
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true, 0
	}

	// Вычисляем задержку до появления хотя бы 1 токена
	needed := 1.0 - b.tokens
	waitSec := needed / b.rate
	retryAfter := time.Duration(waitSec * float64(time.Second))
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return false, retryAfter
}

func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func IPRateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}

			ip := ClientIP(r)
			allowed, retryAfter := limiter.Allow("ip:" + ip)
			if !allowed {
				retrySec := int(retryAfter.Seconds())
				if retrySec <= 0 {
					retrySec = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retrySec))
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "IP rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func UserRateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}

			userID, ok := UserIDFromContext(r.Context())
			if ok {
				allowed, retryAfter := limiter.Allow(fmt.Sprintf("user:%d", userID))
				if !allowed {
					retrySec := int(retryAfter.Seconds())
					if retrySec <= 0 {
						retrySec = 1
					}
					w.Header().Set("Retry-After", strconv.Itoa(retrySec))
					writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "User rate limit exceeded")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func WebhookRateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}

			ip := ClientIP(r)
			allowed, retryAfter := limiter.Allow("webhook:" + ip)
			if !allowed {
				retrySec := int(retryAfter.Seconds())
				if retrySec <= 0 {
					retrySec = 5
				}
				w.Header().Set("Retry-After", strconv.Itoa(retrySec))
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Webhook rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
