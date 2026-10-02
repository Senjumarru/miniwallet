package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/service"
)

func TestProcessWebhook_PaymentFailed(t *testing.T) {
	svc, store, _, secret := setupTestService(t)
	ctx := context.Background()

	// 1. Create a payment in pending status
	pRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "test-webhook-failed-key",
	})
	if err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	payID := pRes.Payment.ID
	ts := time.Now().Unix()
	payload := service.WebhookPayload{
		EventID:           "evt_failed_001",
		EventType:         "payment.failed",
		PaymentID:         payID,
		ProviderPaymentID: fmt.Sprintf("ch_prov_%d", payID),
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	sig := signWebhook(secret, ts, body)

	// Process payment.failed webhook
	if err := svc.ProcessWebhook(ctx, body, sig, fmt.Sprintf("%d", ts)); err != nil {
		t.Fatalf("ProcessWebhook for payment.failed returned error: %v", err)
	}

	// Verify payment status became Failed
	p, err := store.Payments().GetByID(ctx, payID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if p.Status != domain.PaymentStatusFailed {
		t.Fatalf("expected payment status to be failed, got %s", p.Status)
	}

	// Verify payment.failed event was recorded
	events, err := store.PaymentEvents().ListByPaymentID(ctx, payID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	foundFailedEvent := false
	for _, e := range events {
		if e.EventType == "payment.failed" && e.ToStatus == string(domain.PaymentStatusFailed) {
			foundFailedEvent = true
			break
		}
	}
	if !foundFailedEvent {
		t.Fatal("expected payment.failed event to be recorded in repository")
	}

	// Duplicate or subsequent webhook on already failed payment
	ts2 := time.Now().Unix()
	payload2 := service.WebhookPayload{
		EventID:           "evt_failed_002",
		EventType:         "payment.failed",
		PaymentID:         payID,
		ProviderPaymentID: fmt.Sprintf("ch_prov_%d", payID),
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	body2, _ := json.Marshal(payload2)
	sig2 := signWebhook(secret, ts2, body2)
	if err := svc.ProcessWebhook(ctx, body2, sig2, fmt.Sprintf("%d", ts2)); err != nil {
		t.Fatalf("subsequent payment.failed webhook should be handled without error: %v", err)
	}
}

func TestProcessWebhook_PaymentCanceled(t *testing.T) {
	svc, store, _, secret := setupTestService(t)
	ctx := context.Background()

	// 1. Create a payment in pending status
	pRes, err := svc.CreatePayment(ctx, service.CreatePaymentInput{
		UserID:         1,
		OrderID:        10,
		IdempotencyKey: "test-webhook-canceled-key",
	})
	if err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	payID := pRes.Payment.ID
	ts := time.Now().Unix()
	payload := service.WebhookPayload{
		EventID:           "evt_canceled_001",
		EventType:         "payment.canceled",
		PaymentID:         payID,
		ProviderPaymentID: fmt.Sprintf("ch_prov_%d", payID),
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	sig := signWebhook(secret, ts, body)

	// Process payment.canceled webhook
	if err := svc.ProcessWebhook(ctx, body, sig, fmt.Sprintf("%d", ts)); err != nil {
		t.Fatalf("ProcessWebhook for payment.canceled returned error: %v", err)
	}

	// Verify payment status became Canceled
	p, err := store.Payments().GetByID(ctx, payID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if p.Status != domain.PaymentStatusCanceled {
		t.Fatalf("expected payment status to be canceled, got %s", p.Status)
	}

	// Verify payment.canceled event was recorded
	events, err := store.PaymentEvents().ListByPaymentID(ctx, payID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	foundCanceledEvent := false
	for _, e := range events {
		if e.EventType == "payment.canceled" && e.ToStatus == string(domain.PaymentStatusCanceled) {
			foundCanceledEvent = true
			break
		}
	}
	if !foundCanceledEvent {
		t.Fatal("expected payment.canceled event to be recorded in repository")
	}

	// Subsequent webhook on already canceled payment
	ts2 := time.Now().Unix()
	payload2 := service.WebhookPayload{
		EventID:           "evt_canceled_002",
		EventType:         "payment.canceled",
		PaymentID:         payID,
		ProviderPaymentID: fmt.Sprintf("ch_prov_%d", payID),
		AmountMinor:       50000,
		Currency:          "KZT",
	}
	body2, _ := json.Marshal(payload2)
	sig2 := signWebhook(secret, ts2, body2)
	if err := svc.ProcessWebhook(ctx, body2, sig2, fmt.Sprintf("%d", ts2)); err != nil {
		t.Fatalf("subsequent payment.canceled webhook should be handled without error: %v", err)
	}
}
