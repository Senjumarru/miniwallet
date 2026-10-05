package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
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
	mu              sync.Mutex
	buckets         map[string]*tokenBucket
	rate            float64
	burst           int
	clock           domain.Clock
	idleTTL         time.Duration
	cleanupInterval time.Duration
	lastCleanup     time.Time
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
		buckets:         make(map[string]*tokenBucket),
		rate:            rate,
		burst:           burst,
		clock:           clock,
		idleTTL:         3 * time.Minute,
		cleanupInterval: 1 * time.Minute,
	}
}

func (rl *RateLimiter) SetClock(clock domain.Clock) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.clock = clock
}

func (rl *RateLimiter) SetIdleTTL(d time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if d > 0 {
		rl.idleTTL = d
	}
}

func (rl *RateLimiter) SetCleanupInterval(d time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if d > 0 {
		rl.cleanupInterval = d
	}
}

func (rl *RateLimiter) Len() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.buckets)
}

// Cleanup удаляет бакеты, к которым не обращались дольше idleTTL, предотвращая утечку памяти.
func (rl *RateLimiter) Cleanup(idleTTL time.Duration) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.clock.Now().UTC()
	return rl.cleanupLocked(now, idleTTL)
}

func (rl *RateLimiter) cleanupLocked(now time.Time, idleTTL time.Duration) int {
	pruned := 0
	for k, b := range rl.buckets {
		if now.Sub(b.lastUpdate) >= idleTTL {
			delete(rl.buckets, k)
			pruned++
		}
	}
	return pruned
}

// StartBackgroundCleanup запускает периодическую очистку в фоне, завершаясь при отмене ctx.
func (rl *RateLimiter) StartBackgroundCleanup(ctx context.Context, interval, idleTTL time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.Cleanup(idleTTL)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (rl *RateLimiter) Allow(key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now().UTC()

	// Автоматическая очистка старых ключей по расписанию (предотвращение роста памяти)
	if rl.lastCleanup.IsZero() {
		rl.lastCleanup = now
	} else if now.Sub(rl.lastCleanup) >= rl.cleanupInterval {
		rl.cleanupLocked(now, rl.idleTTL)
		rl.lastCleanup = now
	}

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

var (
	trustedProxiesMu   sync.RWMutex
	trustedProxyNets   []*net.IPNet
	trustedProxyInited bool
)

func initTrustedProxies() {
	if trustedProxyInited {
		return
	}
	trustedProxyInited = true

	// По умолчанию доверенными прокси считаются только loopback адреса (127.0.0.1/8, ::1/128)
	cidrs := []string{
		"127.0.0.0/8",
		"::1/128",
	}

	// Переменная окружения TRUSTED_PROXIES позволяет указать дополнительные доверенные сети прокси
	if env := os.Getenv("TRUSTED_PROXIES"); env != "" {
		for _, part := range strings.Split(env, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				cidrs = append(cidrs, part)
			}
		}
	}

	for _, cidr := range cidrs {
		if !strings.Contains(cidr, "/") {
			if ip := net.ParseIP(cidr); ip != nil {
				if ip.To4() != nil {
					cidr += "/32"
				} else {
					cidr += "/128"
				}
			}
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err == nil && ipNet != nil {
			trustedProxyNets = append(trustedProxyNets, ipNet)
		}
	}
}

// SetTrustedProxies позволяет программно настроить список доверенных CIDR или IP прокси.
func SetTrustedProxies(cidrs ...string) {
	trustedProxiesMu.Lock()
	defer trustedProxiesMu.Unlock()

	trustedProxyInited = true
	var nets []*net.IPNet
	// Loopback всегда доверен
	for _, def := range []string{"127.0.0.0/8", "::1/128"} {
		_, ipNet, _ := net.ParseCIDR(def)
		if ipNet != nil {
			nets = append(nets, ipNet)
		}
	}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if ip := net.ParseIP(c); ip != nil {
				if ip.To4() != nil {
					c += "/32"
				} else {
					c += "/128"
				}
			}
		}
		_, ipNet, err := net.ParseCIDR(c)
		if err == nil && ipNet != nil {
			nets = append(nets, ipNet)
		}
	}
	trustedProxyNets = nets
}

func isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	trustedProxiesMu.RLock()
	defer trustedProxiesMu.RUnlock()

	if !trustedProxyInited {
		trustedProxiesMu.RUnlock()
		trustedProxiesMu.Lock()
		initTrustedProxies()
		trustedProxiesMu.Unlock()
		trustedProxiesMu.RLock()
	}

	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP безопасно извлекает IP-адрес клиента:
//  1. Если прямое соединение (RemoteAddr) не от доверенного прокси, заголовки X-Forwarded-For и X-Real-IP игнорируются (защита от спуфинга).
//  2. Если RemoteAddr от доверенного прокси, X-Forwarded-For анализируется справа налево, пропуская промежуточные доверенные прокси,
//     чтобы гарантированно получить реальный клиентский IP, а не поддельное начало списка.
func ClientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || remoteHost == "" {
		remoteHost = r.RemoteAddr
	}

	remoteIP := net.ParseIP(remoteHost)
	if remoteIP == nil || !isTrustedProxy(remoteIP) {
		// Прямое недоверенное соединение: не доверяем заголовкам клиента
		return remoteHost
	}

	// Соединение пришло от доверенного прокси: анализируем X-Forwarded-For справа налево
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ipStr := strings.TrimSpace(parts[i])
			if ipStr == "" {
				continue
			}
			ip := net.ParseIP(ipStr)
			if ip != nil && !isTrustedProxy(ip) {
				return ipStr
			}
		}
		// Если все IP в цепочке доверенные, берём крайний левый валидный
		for _, part := range parts {
			ipStr := strings.TrimSpace(part)
			if net.ParseIP(ipStr) != nil {
				return ipStr
			}
		}
	}

	// Проверяем X-Real-IP
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		if net.ParseIP(xrip) != nil {
			return xrip
		}
	}

	return remoteHost
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
