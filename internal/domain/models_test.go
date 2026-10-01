package domain_test

import (
	"fmt"
	"testing"

	"github.com/Senjumarru/miniwallet/internal/domain"
)

func TestPaymentStatus_TransitionMatrix(t *testing.T) {
	statuses := []domain.PaymentStatus{
		domain.PaymentStatusPending,
		domain.PaymentStatusSucceeded,
		domain.PaymentStatusFailed,
		domain.PaymentStatusCanceled,
	}

	// 16 combinations (4 x 4)
	expectedAllowed := map[string]bool{
		"pending->succeeded": true,
		"pending->failed":    true,
		"pending->canceled":  true,
	}

	count := 0
	for _, from := range statuses {
		for _, to := range statuses {
			count++
			pair := fmt.Sprintf("%s->%s", from, to)
			wantAllowed := expectedAllowed[pair]
			gotAllowed := from.CanTransitionTo(to)

			t.Run(pair, func(t *testing.T) {
				if gotAllowed != wantAllowed {
					t.Errorf("transition %s: got allowed=%v, want=%v", pair, gotAllowed, wantAllowed)
				}
			})
		}
	}

	if count != 16 {
		t.Fatalf("expected 16 transitions tested, got %d", count)
	}
}
