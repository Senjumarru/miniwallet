package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

type dummyNetError struct {
	timeout   bool
	temporary bool
}

func (e *dummyNetError) Error() string   { return "dummy network error" }
func (e *dummyNetError) Timeout() bool   { return e.timeout }
func (e *dummyNetError) Temporary() bool { return e.temporary }

func TestIsNetworkOrTimeout(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "regular application error",
			err:      errors.New("some application logic error"),
			expected: false,
		},
		{
			name:     "context.DeadlineExceeded",
			err:      context.DeadlineExceeded,
			expected: true,
		},
		{
			name:     "wrapped context.DeadlineExceeded",
			err:      fmt.Errorf("wrap: %w", context.DeadlineExceeded),
			expected: true,
		},
		{
			name:     "context.Canceled",
			err:      context.Canceled,
			expected: true,
		},
		{
			name:     "wrapped context.Canceled",
			err:      fmt.Errorf("wrap: %w", context.Canceled),
			expected: true,
		},
		{
			name:     "net.OpError",
			err:      &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			expected: true,
		},
		{
			name:     "net.DNSError",
			err:      &net.DNSError{Err: "no such host", Name: "fake.invalid"},
			expected: true,
		},
		{
			name:     "custom net.Error implementation",
			err:      &dummyNetError{timeout: true},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isNetworkOrTimeout(tc.err)
			if got != tc.expected {
				t.Errorf("isNetworkOrTimeout(%v) = %v; want %v", tc.err, got, tc.expected)
			}
		})
	}
}

func TestErrorClassification_DefinitiveAndAmbiguous(t *testing.T) {
	t.Run("IsDefinitiveRejection", func(t *testing.T) {
		if !IsDefinitiveRejection(ErrDefinitiveRejection) {
			t.Error("expected true for ErrDefinitiveRejection")
		}
		if !IsDefinitiveRejection(ErrProviderClientError) {
			t.Error("expected true for ErrProviderClientError")
		}
		wrapped := fmt.Errorf("outer: %w", ErrDefinitiveRejection)
		if !IsDefinitiveRejection(wrapped) {
			t.Error("expected true for wrapped ErrDefinitiveRejection")
		}
		if IsDefinitiveRejection(ErrAmbiguousOutcome) {
			t.Error("expected false for ErrAmbiguousOutcome")
		}
		if IsDefinitiveRejection(ErrProviderUnavailable) {
			t.Error("expected false for ErrProviderUnavailable")
		}
		if IsDefinitiveRejection(errors.New("other")) {
			t.Error("expected false for random error")
		}
	})

	t.Run("IsAmbiguousOutcome", func(t *testing.T) {
		if !IsAmbiguousOutcome(ErrAmbiguousOutcome) {
			t.Error("expected true for ErrAmbiguousOutcome")
		}
		if !IsAmbiguousOutcome(ErrProviderUnavailable) {
			t.Error("expected true for ErrProviderUnavailable")
		}
		wrapped := fmt.Errorf("outer: %w", ErrAmbiguousOutcome)
		if !IsAmbiguousOutcome(wrapped) {
			t.Error("expected true for wrapped ErrAmbiguousOutcome")
		}
		if IsAmbiguousOutcome(ErrDefinitiveRejection) {
			t.Error("expected false for ErrDefinitiveRejection")
		}
		if IsAmbiguousOutcome(ErrProviderClientError) {
			t.Error("expected false for ErrProviderClientError")
		}
		if IsAmbiguousOutcome(errors.New("other")) {
			t.Error("expected false for random error")
		}
	})
}
