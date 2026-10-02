package provider_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/provider"
)

func TestClient_GetPaymentStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("200 OK statuses", func(t *testing.T) {
		statusCases := []struct {
			jsonResp       string
			expectedStatus domain.PaymentStatus
		}{
			{`{"status":"succeeded"}`, domain.PaymentStatusSucceeded},
			{`{"status":"failed"}`, domain.PaymentStatusFailed},
			{`{"status":"canceled"}`, domain.PaymentStatusCanceled},
			{`{"status":"pending"}`, domain.PaymentStatusPending},
			{`{"status":"unknown_xyz"}`, domain.PaymentStatusPending},
		}

		for _, tc := range statusCases {
			t.Run(string(tc.expectedStatus)+"_"+tc.jsonResp, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/payments/ch_test_123" {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(tc.jsonResp))
				}))
				defer server.Close()

				client := provider.NewClient(
					server.URL,
					server.Client(),
					logger,
					time.Second,
					1,
					10*time.Millisecond,
					50*time.Millisecond,
				)

				st, err := client.GetPaymentStatus(context.Background(), "ch_test_123")
				if err != nil {
					t.Fatalf("unexpected error for 200 OK: %v", err)
				}
				if st != tc.expectedStatus {
					t.Errorf("expected status %s, got %s", tc.expectedStatus, st)
				}
			})
		}
	})

	t.Run("404 Not Found returns PaymentStatusFailed without error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		client := provider.NewClient(
			server.URL,
			server.Client(),
			logger,
			time.Second,
			1,
			10*time.Millisecond,
			50*time.Millisecond,
		)

		st, err := client.GetPaymentStatus(context.Background(), "ch_not_found")
		if err != nil {
			t.Fatalf("expected nil error on 404, got: %v", err)
		}
		if st != domain.PaymentStatusFailed {
			t.Errorf("expected status %s on 404, got %s", domain.PaymentStatusFailed, st)
		}
	})

	t.Run("5xx Server Error returns error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"internal_server_error"}`))
		}))
		defer server.Close()

		client := provider.NewClient(
			server.URL,
			server.Client(),
			logger,
			time.Second,
			1,
			10*time.Millisecond,
			50*time.Millisecond,
		)

		st, err := client.GetPaymentStatus(context.Background(), "ch_err_500")
		if err == nil {
			t.Fatal("expected error on 500, got nil")
		}
		if st != "" {
			t.Errorf("expected empty status on error, got %s", st)
		}
	})

	t.Run("Timeout returns network error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := provider.NewClient(
			server.URL,
			server.Client(),
			logger,
			50*time.Millisecond, // Client timeout 50ms < server response 200ms
			1,
			10*time.Millisecond,
			50*time.Millisecond,
		)

		st, err := client.GetPaymentStatus(context.Background(), "ch_timeout")
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}
		if st != "" {
			t.Errorf("expected empty status on timeout, got %s", st)
		}
	})

	t.Run("Invalid JSON response returns decode error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`invalid-json-response`))
		}))
		defer server.Close()

		client := provider.NewClient(
			server.URL,
			server.Client(),
			logger,
			time.Second,
			1,
			10*time.Millisecond,
			50*time.Millisecond,
		)

		st, err := client.GetPaymentStatus(context.Background(), "ch_bad_json")
		if err == nil {
			t.Fatal("expected json decode error, got nil")
		}
		if st != "" {
			t.Errorf("expected empty status on error, got %s", st)
		}
	})
}
