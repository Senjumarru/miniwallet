package domain

import "time"

// Clock предоставляет интерфейс для получения времени.
// Позволяет замораживать или сдвигать время в тестах.
type Clock interface {
	Now() time.Time
}

// RealClock возвращает текущее реальное системное время в формате UTC.
type RealClock struct{}

func (RealClock) Now() time.Time {
	return time.Now().UTC()
}

// FrozenClock возвращает фиксированное время (для детерминированных тестов).
type FrozenClock struct {
	Current time.Time
}

func (f FrozenClock) Now() time.Time {
	return f.Current.UTC()
}
