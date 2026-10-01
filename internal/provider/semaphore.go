package provider

import (
	"context"
	"errors"
)

var ErrProviderOverloaded = errors.New("provider overloaded: max concurrent requests reached")

type Semaphore struct {
	ch chan struct{}
}

func NewSemaphore(maxConcurrent int) *Semaphore {
	if maxConcurrent <= 0 {
		maxConcurrent = 20
	}
	return &Semaphore{
		ch: make(chan struct{}, maxConcurrent),
	}
}

func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrProviderOverloaded
	}
}

func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Semaphore) Release() {
	select {
	case <-s.ch:
	default:
	}
}

func (s *Semaphore) Available() int {
	return cap(s.ch) - len(s.ch)
}
