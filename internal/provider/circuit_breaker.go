package provider

import (
	"errors"
	"sync"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

var ErrCircuitOpen = errors.New("circuit breaker open: provider temporarily unavailable")

type CircuitState int

const (
	StateClosed CircuitState = iota
	StateHalfOpen
	StateOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half-open"
	case StateOpen:
		return "open"
	default:
		return "unknown"
	}
}

type CircuitBreakerConfig struct {
	FailureThreshold int
	Cooldown         time.Duration
	SuccessThreshold int
}

type CircuitBreaker struct {
	mu               sync.Mutex
	state            CircuitState
	cfg              CircuitBreakerConfig
	consecutiveFails int
	consecutivePass  int
	lastOpenedAt     time.Time
	clock            domain.Clock
}

func NewCircuitBreaker(failureThreshold int, cooldown time.Duration, successThreshold int, clock domain.Clock) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if cooldown <= 0 {
		cooldown = 5 * time.Second
	}
	if successThreshold <= 0 {
		successThreshold = 1
	}
	if clock == nil {
		clock = domain.RealClock{}
	}
	return &CircuitBreaker{
		state: StateClosed,
		cfg: CircuitBreakerConfig{
			FailureThreshold: failureThreshold,
			Cooldown:         cooldown,
			SuccessThreshold: successThreshold,
		},
		clock: clock,
	}
}

func (cb *CircuitBreaker) SetClock(clock domain.Clock) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.clock = clock
}

func (cb *CircuitBreaker) now() time.Time {
	if cb.clock != nil {
		return cb.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Allow проверяет состояние цепи. Если Open и прошло время Cooldown — переводит в HalfOpen.
// Если Open и Cooldown не прошел — возвращает ErrCircuitOpen (503).
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.now()

	switch cb.state {
	case StateOpen:
		if now.Sub(cb.lastOpenedAt) >= cb.cfg.Cooldown {
			cb.state = StateHalfOpen
			cb.consecutivePass = 0
			return nil
		}
		return ErrCircuitOpen

	case StateHalfOpen:
		// В состоянии half-open пропускаем пробный запрос
		return nil

	case StateClosed:
		return nil
	}

	return nil
}

func (cb *CircuitBreaker) OnSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateHalfOpen {
		cb.consecutivePass++
		if cb.consecutivePass >= cb.cfg.SuccessThreshold {
			cb.state = StateClosed
			cb.consecutiveFails = 0
			cb.consecutivePass = 0
		}
	} else if cb.state == StateClosed {
		cb.consecutiveFails = 0
	}
}

func (cb *CircuitBreaker) OnFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.now()

	if cb.state == StateHalfOpen {
		// Ошибка в half-open сразу возвращает цепь в Open
		cb.state = StateOpen
		cb.lastOpenedAt = now
		cb.consecutivePass = 0
	} else if cb.state == StateClosed {
		cb.consecutiveFails++
		if cb.consecutiveFails >= cb.cfg.FailureThreshold {
			cb.state = StateOpen
			cb.lastOpenedAt = now
		}
	}
}
