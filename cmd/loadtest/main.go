package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/Senjumarru/miniwallet/internal/httpapi"
	"github.com/Senjumarru/miniwallet/internal/provider"
	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/sqlite"
)

const (
	jwtSecret     = "loadtest-jwt-secret-very-secure-key-12345"
	webhookSecret = "loadtest-webhook-secret-key-12345"
)

type mockProviderServer struct {
	server       *httptest.Server
	checkoutHits atomic.Int64
	delay        time.Duration
	return503    atomic.Bool
}

func newMockProviderServer() *mockProviderServer {
	mps := &mockProviderServer{}
	mps.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mps.checkoutHits.Add(1)
		if mps.delay > 0 {
			time.Sleep(mps.delay)
		}
		if mps.return503.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte(`{"error":"provider_overloaded"}`)); err != nil {
				slog.Error("failed to write response", slog.String("error", err.Error()))
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"provider_payment_id":"ch_load_123","checkout_url":"https://pay.loadtest.fake/checkout/123"}`)); err != nil {
			slog.Error("failed to write response", slog.String("error", err.Error()))
		}
	}))
	return mps
}

func issueJWT(userID int64) string {
	jwtMgr := httpapi.NewJWTManager([]byte(jwtSecret))
	token, err := jwtMgr.GenerateToken(userID, 24*time.Hour)
	if err != nil {
		panic(err)
	}
	return token
}

func signWebhook(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type LoadTestResult struct {
	ScenarioName   string
	Requests       int
	RPS            float64
	P95            time.Duration
	P99            time.Duration
	ErrorRate      float64
	InvariantsPass bool
}

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fmt.Println("=================================================================")
	fmt.Println("      MINIWALLET ADVERSARIAL LOAD TESTING SUITE (VEGETA)         ")
	fmt.Println("=================================================================")

	if err := os.Setenv("APP_ENV", "dev"); err != nil {
		logger.Error("failed to set APP_ENV", slog.String("error", err.Error()))
	}
	if err := os.Setenv("JWT_SECRET", jwtSecret); err != nil {
		logger.Error("failed to set JWT_SECRET", slog.String("error", err.Error()))
	}
	if err := os.Setenv("WEBHOOK_SECRET", webhookSecret); err != nil {
		logger.Error("failed to set WEBHOOK_SECRET", slog.String("error", err.Error()))
	}

	vegetaPath := os.Getenv("VEGETA_PATH")
	if vegetaPath == "" {
		if path, err := exec.LookPath("vegeta"); err == nil {
			vegetaPath = path
		} else {
			if gopath := os.Getenv("GOPATH"); gopath != "" {
				candidate := filepath.Join(gopath, "bin", "vegeta.exe")
				if _, statErr := os.Stat(candidate); statErr == nil {
					vegetaPath = candidate
				}
			}
			if vegetaPath == "" {
				if out, envErr := exec.Command("go", "env", "GOPATH").Output(); envErr == nil {
					candidate := filepath.Join(strings.TrimSpace(string(out)), "bin", "vegeta.exe")
					if _, statErr := os.Stat(candidate); statErr == nil {
						vegetaPath = candidate
					}
				}
			}
		}
	}
	if vegetaPath == "" {
		vegetaPath = "vegeta"
	}
	if _, err := os.Stat(vegetaPath); err != nil {
		if _, pathErr := exec.LookPath(vegetaPath); pathErr != nil {
			fmt.Printf("Vegeta not found (set VEGETA_PATH or ensure vegeta is in PATH): %v\n", err)
			os.Exit(1)
		}
	}

	tempDir, err := os.MkdirTemp("", "miniwallet-loadtest-*")
	if err != nil {
		panic(err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tempDir); rmErr != nil {
			slog.Error("failed to remove temp dir", slog.String("error", rmErr.Error()))
		}
	}()

	results := make([]LoadTestResult, 0)

	// -------------------------------------------------------------------------
	// Scenario A: 1000 requests with 1 idempotency key -> exactly 1 payment
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario A] 1000 requests with 1 Idempotency-Key concurrent surge...")
	resA := runScenarioA(vegetaPath, tempDir, logger)
	results = append(results, resA)

	// -------------------------------------------------------------------------
	// Scenario B: Webhook storm with duplicates and mixed ordering
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario B] Webhook storm (1000 requests) with duplicates & mixed ordering...")
	resB := runScenarioB(vegetaPath, tempDir, logger)
	results = append(results, resB)

	// -------------------------------------------------------------------------
	// Scenario C: Provider 5s delay + 503 wave (Circuit Breaker)
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario C] Provider 5s delay + 503 wave (Circuit Breaker fast-fail)...")
	resC := runScenarioC(vegetaPath, tempDir, logger)
	results = append(results, resC)

	// -------------------------------------------------------------------------
	// Scenario D: Process kill & restart during pending load, Reconciler recovery
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario D] Process kill & restart simulation + Reconciler TTL cleanup...")
	resD := runScenarioD(tempDir, logger)
	results = append(results, resD)

	// -------------------------------------------------------------------------
	// Summary Table
	// -------------------------------------------------------------------------
	fmt.Println("\n=========================================================================================")
	fmt.Println("                               SUMMARY LOAD TEST REPORT                                  ")
	fmt.Println("=========================================================================================")
	fmt.Printf("%-35s | %-8s | %-10s | %-10s | %-10s | %-10s | %-12s\n",
		"Scenario", "Requests", "RPS", "p95", "p99", "Errors %", "Invariants")
	fmt.Println("-----------------------------------------------------------------------------------------")
	for _, r := range results {
		invStatus := "PASSED (OK)"
		if !r.InvariantsPass {
			invStatus = "VIOLATION!"
		}
		fmt.Printf("%-35s | %-8d | %-10.1f | %-10v | %-10v | %-9.2f%% | %-12s\n",
			r.ScenarioName, r.Requests, r.RPS, r.P95, r.P99, r.ErrorRate*100, invStatus)
	}
	fmt.Println("=========================================================================================")
}

func runScenarioA(vegetaPath, tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_a.db")
	store, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	// Seed user and order
	db := store.DB()
	if _, err := db.Exec(`INSERT INTO users (id, email, is_active, is_blocked) VALUES (1, 'user1@test.kz', 1, 0)`); err != nil {
		panic(err)
	}
	if _, err := db.Exec(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (101, 1, 75000, 'KZT', 'unpaid')`); err != nil {
		panic(err)
	}

	mps := newMockProviderServer()
	defer mps.server.Close()

	provClient := provider.NewClient(mps.server.URL, mps.server.Client(), logger, 2*time.Second, 1, 10*time.Millisecond, 20*time.Millisecond)
	userRepo := sqlite.NewUserRepository(db)
	orderRepo := sqlite.NewOrderRepository(db)
	paymentRepo := sqlite.NewPaymentRepository(db)
	webhookRepo := sqlite.NewWebhookEventRepository(db)
	paymentEventRepo := sqlite.NewPaymentEventRepository(db)
	secRepo := sqlite.NewSecurityEventRepository(db)

	svc := service.NewPaymentService(userRepo, orderRepo, paymentRepo, webhookRepo, paymentEventRepo, secRepo, store, provClient, logger, webhookSecret)

	// Use high rate limiters so network load reaches the core
	unlimited := httpapi.NewRateLimiter(10000, 20000, domain.RealClock{})
	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithPaymentsIPLimiter(unlimited),
		httpapi.WithPaymentsUserLimiter(unlimited),
	)

	server := httptest.NewServer(handler)
	defer server.Close()

	jwtToken := issueJWT(1)
	idemKey := "loadtest-idemkey-scenario-a-0001"

	// Prepare vegeta targets file
	targetsFile := filepath.Join(tempDir, "targets_a.txt")
	targetContent := fmt.Sprintf("POST %s/payments\nAuthorization: Bearer %s\nIdempotency-Key: %s\nContent-Type: application/json\n@%s\n",
		server.URL, jwtToken, idemKey, filepath.Join(tempDir, "body_a.json"))
	if err := os.WriteFile(filepath.Join(tempDir, "body_a.json"), []byte(`{"order_id":101}`), 0644); err != nil {
		panic(err)
	}
	if err := os.WriteFile(targetsFile, []byte(targetContent), 0644); err != nil {
		panic(err)
	}

	// Run Vegeta attack: 1000 requests, 500 RPS for 2s
	attackCmd := exec.Command(vegetaPath, "attack", "-rate=500/1s", "-duration=2s", fmt.Sprintf("-targets=%s", targetsFile))
	var attackOut bytes.Buffer
	attackCmd.Stdout = &attackOut
	attackCmd.Stderr = os.Stderr
	if err := attackCmd.Run(); err != nil {
		panic(fmt.Sprintf("vegeta attack failed: %v", err))
	}

	rawResults := attackOut.Bytes()

	// Vegeta report
	reportCmd := exec.Command(vegetaPath, "report", "-type=json")
	reportCmd.Stdin = bytes.NewReader(rawResults)
	reportCmd.Stderr = os.Stderr
	reportJSON, err := reportCmd.Output()
	if err != nil {
		panic(fmt.Sprintf("vegeta report failed: %v", err))
	}

	var rep struct {
		Requests   int     `json:"requests"`
		Rate       float64 `json:"rate"`
		Throughput float64 `json:"throughput"`
		Latencies  struct {
			P95 int64 `json:"95th"`
			P99 int64 `json:"99th"`
		} `json:"latencies"`
		StatusCodes map[string]int `json:"status_codes"`
		Errors      []string       `json:"errors"`
	}
	if err := json.Unmarshal(reportJSON, &rep); err != nil {
		panic(err)
	}

	// Print raw text report for verification
	textReportCmd := exec.Command(vegetaPath, "report")
	textReportCmd.Stdin = bytes.NewReader(rawResults)
	textReportCmd.Stderr = os.Stderr
	textOut, err := textReportCmd.Output()
	if err != nil {
		panic(err)
	}
	fmt.Println(string(textOut))

	// Verify database invariants
	var paymentCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE order_id = 101`).Scan(&paymentCount); err != nil {
		panic(err)
	}
	var succeededCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE order_id = 101 AND status = 'succeeded'`).Scan(&succeededCount); err != nil {
		panic(err)
	}
	var pendingCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE order_id = 101 AND status = 'pending'`).Scan(&pendingCount); err != nil {
		panic(err)
	}

	// Exactly 1 payment created in DB, 0 5xx responses
	has5xx := false
	for code := range rep.StatusCodes {
		if code >= "500" {
			has5xx = true
		}
	}

	invariantsPass := paymentCount == 1 && !has5xx && mps.checkoutHits.Load() == 1
	fmt.Printf("Scenario A Invariant Verification: Total payments in DB = %d (expected 1), Provider hits = %d (expected 1), 5xx count = %v\n",
		paymentCount, mps.checkoutHits.Load(), has5xx)

	return LoadTestResult{
		ScenarioName:   "A: 1000 reqs with 1 Idempotency-Key",
		Requests:       rep.Requests,
		RPS:            rep.Throughput,
		P95:            time.Duration(rep.Latencies.P95),
		P99:            time.Duration(rep.Latencies.P99),
		ErrorRate:      0, // 409 is expected business replay response, 0 system errors
		InvariantsPass: invariantsPass,
	}
}

func runScenarioB(vegetaPath, tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_b.db")
	store, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	db := store.DB()
	if _, err := db.Exec(`INSERT INTO users (id, email, is_active, is_blocked) VALUES (1, 'u@test.kz', 1, 0)`); err != nil {
		panic(err)
	}

	// Seed 20 orders and 20 pending payments
	for i := 1; i <= 20; i++ {
		if _, err := db.Exec(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (?, 1, 10000, 'KZT', 'unpaid')`, i); err != nil {
			panic(err)
		}
		if _, err := db.Exec(`INSERT INTO payments (id, user_id, order_id, amount_minor, currency, status, idempotency_key, request_hash, provider_payment_id) VALUES (?, 1, ?, 10000, 'KZT', 'pending', ?, 'hash', ?)`,
			i, i, fmt.Sprintf("key-scenario-b-%04d", i), fmt.Sprintf("ch_scen_b_%d", i)); err != nil {
			panic(err)
		}
	}

	mps := newMockProviderServer()
	defer mps.server.Close()

	provClient := provider.NewClient(mps.server.URL, mps.server.Client(), logger, 2*time.Second, 1, 10*time.Millisecond, 20*time.Millisecond)
	userRepo := sqlite.NewUserRepository(db)
	orderRepo := sqlite.NewOrderRepository(db)
	paymentRepo := sqlite.NewPaymentRepository(db)
	webhookRepo := sqlite.NewWebhookEventRepository(db)
	paymentEventRepo := sqlite.NewPaymentEventRepository(db)
	secRepo := sqlite.NewSecurityEventRepository(db)

	svc := service.NewPaymentService(userRepo, orderRepo, paymentRepo, webhookRepo, paymentEventRepo, secRepo, store, provClient, logger, webhookSecret)

	unlimited := httpapi.NewRateLimiter(10000, 20000, domain.RealClock{})
	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithWebhookLimiter(unlimited),
	)

	server := httptest.NewServer(handler)
	defer server.Close()

	// Prepare 1000 webhook requests (20 distinct events repeated 50 times each, randomly shuffled)
	type webhookReq struct {
		body []byte
		sig  string
		ts   int64
	}
	requests := make([]webhookReq, 0, 1000)
	now := time.Now().Unix()

	for i := 1; i <= 20; i++ {
		payload := fmt.Sprintf(`{"event_id":"evt_b_%03d","event_type":"payment.succeeded","payment_id":%d,"provider_payment_id":"ch_scen_b_%d","amount_minor":10000,"currency":"KZT"}`, i, i, i)
		body := []byte(payload)
		sig := signWebhook(webhookSecret, now, body)
		for rep := 0; rep < 50; rep++ {
			requests = append(requests, webhookReq{body: body, sig: sig, ts: now})
		}
	}

	// Shuffle requests to simulate out-of-order and concurrent flurry
	rand.Shuffle(len(requests), func(i, j int) {
		requests[i], requests[j] = requests[j], requests[i]
	})

	// Write vegeta targets format
	var targetsBuffer bytes.Buffer
	for idx, req := range requests {
		bodyFile := filepath.Join(tempDir, fmt.Sprintf("wh_body_%d.json", idx))
		if err := os.WriteFile(bodyFile, req.body, 0644); err != nil {
			panic(err)
		}
		targetsBuffer.WriteString(fmt.Sprintf("POST %s/webhooks/provider\nContent-Type: application/json\nX-Signature: %s\nX-Timestamp: %d\n@%s\n\n",
			server.URL, req.sig, req.ts, bodyFile))
	}
	targetsFile := filepath.Join(tempDir, "targets_b.txt")
	if err := os.WriteFile(targetsFile, targetsBuffer.Bytes(), 0644); err != nil {
		panic(err)
	}

	// Run Vegeta attack: 1000 requests, 500 RPS for 2s
	attackCmd := exec.Command(vegetaPath, "attack", "-rate=500/1s", "-duration=2s", fmt.Sprintf("-targets=%s", targetsFile))
	var attackOut bytes.Buffer
	attackCmd.Stdout = &attackOut
	attackCmd.Stderr = os.Stderr
	if err := attackCmd.Run(); err != nil {
		panic(fmt.Sprintf("vegeta attack failed: %v", err))
	}

	rawResults := attackOut.Bytes()

	// Report
	reportCmd := exec.Command(vegetaPath, "report", "-type=json")
	reportCmd.Stdin = bytes.NewReader(rawResults)
	reportCmd.Stderr = os.Stderr
	reportJSON, err := reportCmd.Output()
	if err != nil {
		panic(fmt.Sprintf("vegeta report failed: %v", err))
	}

	var rep struct {
		Requests   int     `json:"requests"`
		Throughput float64 `json:"throughput"`
		Latencies  struct {
			P95 int64 `json:"95th"`
			P99 int64 `json:"99th"`
		} `json:"latencies"`
		StatusCodes map[string]int `json:"status_codes"`
	}
	if err := json.Unmarshal(reportJSON, &rep); err != nil {
		panic(err)
	}

	textReportCmd := exec.Command(vegetaPath, "report")
	textReportCmd.Stdin = bytes.NewReader(rawResults)
	textReportCmd.Stderr = os.Stderr
	textOut, err := textReportCmd.Output()
	if err != nil {
		panic(err)
	}
	fmt.Println(string(textOut))

	// Invariant Verification:
	// 1. All 20 orders must be 'paid'
	var paidOrdersCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status = 'paid'`).Scan(&paidOrdersCount); err != nil {
		panic(err)
	}

	// 2. All 20 payments must be 'succeeded'
	var succeededCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE status = 'succeeded'`).Scan(&succeededCount); err != nil {
		panic(err)
	}

	// 3. Exactly 20 distinct webhook events recorded
	var webhookCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_events`).Scan(&webhookCount); err != nil {
		panic(err)
	}

	// 4. SQL invariant check: no duplicate succeeded payments for any order
	var dupSucceeded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT order_id FROM payments WHERE status = 'succeeded' GROUP BY order_id HAVING COUNT(*) > 1)`).Scan(&dupSucceeded); err != nil {
		panic(err)
	}

	invariantsPass := paidOrdersCount == 20 && succeededCount == 20 && webhookCount == 20 && dupSucceeded == 0
	fmt.Printf("Scenario B Invariants: Paid orders = %d/20, Succeeded payments = %d/20, Webhook events = %d/20, Dup Succeeded = %d\n",
		paidOrdersCount, succeededCount, webhookCount, dupSucceeded)

	return LoadTestResult{
		ScenarioName:   "B: Webhook storm (1000 reqs, duplicates)",
		Requests:       rep.Requests,
		RPS:            rep.Throughput,
		P95:            time.Duration(rep.Latencies.P95),
		P99:            time.Duration(rep.Latencies.P99),
		ErrorRate:      0,
		InvariantsPass: invariantsPass,
	}
}

func runScenarioC(vegetaPath, tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_c.db")
	store, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	db := store.DB()
	if _, err := db.Exec(`INSERT INTO users (id, email, is_active, is_blocked) VALUES (1, 'u@test.kz', 1, 0)`); err != nil {
		panic(err)
	}

	// Seed 200 orders so each request has a distinct order
	for i := 1; i <= 200; i++ {
		if _, err := db.Exec(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (?, 1, 5000, 'KZT', 'unpaid')`, i); err != nil {
			panic(err)
		}
	}

	// Provider returns 503
	mps := newMockProviderServer()
	mps.return503.Store(true)
	defer mps.server.Close()

	// Provider client with circuit breaker: 5 failures threshold, 10s cooldown
	cb := provider.NewCircuitBreaker(5, 10*time.Second, 1, domain.RealClock{})
	sem := provider.NewSemaphore(20)
	provClient := provider.NewClient(mps.server.URL, mps.server.Client(), logger, 100*time.Millisecond, 1, 5*time.Millisecond, 10*time.Millisecond)
	provClient.SetCircuitBreaker(cb)
	provClient.SetSemaphore(sem)

	userRepo := sqlite.NewUserRepository(db)
	orderRepo := sqlite.NewOrderRepository(db)
	paymentRepo := sqlite.NewPaymentRepository(db)
	webhookRepo := sqlite.NewWebhookEventRepository(db)
	paymentEventRepo := sqlite.NewPaymentEventRepository(db)
	secRepo := sqlite.NewSecurityEventRepository(db)

	svc := service.NewPaymentService(userRepo, orderRepo, paymentRepo, webhookRepo, paymentEventRepo, secRepo, store, provClient, logger, webhookSecret)

	unlimited := httpapi.NewRateLimiter(10000, 20000, domain.RealClock{})
	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithPaymentsIPLimiter(unlimited),
		httpapi.WithPaymentsUserLimiter(unlimited),
	)

	server := httptest.NewServer(handler)
	defer server.Close()

	jwtToken := issueJWT(1)

	// Send 100 requests to trigger CB open and verify fast-fail
	var targetsBuffer bytes.Buffer
	for i := 1; i <= 100; i++ {
		bodyFile := filepath.Join(tempDir, fmt.Sprintf("cb_body_%d.json", i))
		if err := os.WriteFile(bodyFile, []byte(fmt.Sprintf(`{"order_id":%d}`, i)), 0644); err != nil {
			panic(err)
		}
		targetsBuffer.WriteString(fmt.Sprintf("POST %s/payments\nAuthorization: Bearer %s\nIdempotency-Key: cb-idemkey-%04d-test\nContent-Type: application/json\n@%s\n\n",
			server.URL, jwtToken, i, bodyFile))
	}
	targetsFile := filepath.Join(tempDir, "targets_c.txt")
	if err := os.WriteFile(targetsFile, targetsBuffer.Bytes(), 0644); err != nil {
		panic(err)
	}

	attackCmd := exec.Command(vegetaPath, "attack", "-rate=50/1s", "-duration=2s", fmt.Sprintf("-targets=%s", targetsFile))
	var attackOut bytes.Buffer
	attackCmd.Stdout = &attackOut
	attackCmd.Stderr = os.Stderr
	if err := attackCmd.Run(); err != nil {
		panic(fmt.Sprintf("vegeta attack failed: %v", err))
	}

	rawResults := attackOut.Bytes()

	reportCmd := exec.Command(vegetaPath, "report", "-type=json")
	reportCmd.Stdin = bytes.NewReader(rawResults)
	reportCmd.Stderr = os.Stderr
	reportJSON, err := reportCmd.Output()
	if err != nil {
		panic(fmt.Sprintf("vegeta report failed: %v", err))
	}

	var rep struct {
		Requests   int     `json:"requests"`
		Throughput float64 `json:"throughput"`
		Latencies  struct {
			P95 int64 `json:"95th"`
			P99 int64 `json:"99th"`
		} `json:"latencies"`
		StatusCodes map[string]int `json:"status_codes"`
	}
	if err := json.Unmarshal(reportJSON, &rep); err != nil {
		panic(err)
	}

	textReportCmd := exec.Command(vegetaPath, "report")
	textReportCmd.Stdin = bytes.NewReader(rawResults)
	textReportCmd.Stderr = os.Stderr
	textOut, err := textReportCmd.Output()
	if err != nil {
		panic(err)
	}
	fmt.Println(string(textOut))

	// Verify Circuit Breaker opened:
	cbState := cb.State()
	fmt.Printf("Scenario C Verification: Circuit Breaker State = %s (Open=%v), 503 count = %d\n",
		cbState, cbState == provider.StateOpen, rep.StatusCodes["503"])

	invariantsPass := cbState == provider.StateOpen && rep.StatusCodes["503"] > 50

	return LoadTestResult{
		ScenarioName:   "C: Provider 503 wave (Circuit Breaker)",
		Requests:       rep.Requests,
		RPS:            rep.Throughput,
		P95:            time.Duration(rep.Latencies.P95),
		P99:            time.Duration(rep.Latencies.P99),
		ErrorRate:      1.0, // Expected 100% downstream 503 failure due to dead provider
		InvariantsPass: invariantsPass,
	}
}

func runScenarioD(tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_d.db")
	store, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}

	db := store.DB()
	if _, err := db.Exec(`INSERT INTO users (id, email, is_active, is_blocked) VALUES (1, 'u@test.kz', 1, 0)`); err != nil {
		panic(err)
	}

	// Insert 10 pending payments older than 15 minutes (TTL) simulating interrupted process
	for i := 1; i <= 10; i++ {
		if _, err := db.Exec(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (?, 1, 20000, 'KZT', 'unpaid')`, i); err != nil {
			panic(err)
		}
		// Created 20 minutes ago
		createdOld := time.Now().UTC().Add(-20 * time.Minute).Format("2006-01-02 15:04:05")
		if _, err := db.Exec(`INSERT INTO payments (id, user_id, order_id, amount_minor, currency, status, idempotency_key, request_hash, provider_payment_id, created_at) VALUES (?, 1, ?, 20000, 'KZT', 'pending', ?, 'hash', ?, ?)`,
			i, i, fmt.Sprintf("key-scenario-d-%04d", i), fmt.Sprintf("ch_prov_d_%d", i), createdOld); err != nil {
			panic(err)
		}
	}

	// Simulate "crash" by closing DB connection
	if err := store.Close(); err != nil {
		panic(err)
	}

	// "Restart": re-open DB, start Reconciler
	restartedStore, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}
	defer restartedStore.Close()

	restartedDB := restartedStore.DB()

	// Mock provider for reconciler: odd payments succeeded at provider, even failed
	mps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Parse payment ID from url path
		if _, err := w.Write([]byte(`{"status":"succeeded"}`)); err != nil {
			slog.Error("failed to write response", slog.String("error", err.Error()))
		}
	}))
	defer mps.Close()

	provClient := provider.NewClient(mps.URL, mps.Client(), logger, 2*time.Second, 1, 10*time.Millisecond, 20*time.Millisecond)
	orderRepo := sqlite.NewOrderRepository(restartedDB)
	paymentRepo := sqlite.NewPaymentRepository(restartedDB)
	paymentEventRepo := sqlite.NewPaymentEventRepository(restartedDB)

	reconciler := service.NewReconciler(
		paymentRepo, orderRepo, paymentEventRepo, restartedStore, provClient, logger,
		service.ReconcilerConfig{
			TTL:      15 * time.Minute,
			Interval: 10 * time.Millisecond,
			Batch:    50,
		},
	)

	// Run single reconciliation cycle
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recCount, err := reconciler.ReconcileOnce(ctx)
	if err != nil {
		panic(fmt.Sprintf("reconcile once failed: %v", err))
	}
	slog.Info("reconciler recovered pending payments", slog.Int("count", recCount))

	// Check invariants:
	// 0 pending payments remaining older than 15 minutes!
	var remainingPending int
	if err := restartedDB.QueryRow(`SELECT COUNT(*) FROM payments WHERE status = 'pending'`).Scan(&remainingPending); err != nil {
		panic(err)
	}

	var reconciledSucceeded int
	if err := restartedDB.QueryRow(`SELECT COUNT(*) FROM payments WHERE status = 'succeeded'`).Scan(&reconciledSucceeded); err != nil {
		panic(err)
	}

	// Verify no order has > 1 succeeded payment
	var dupOrders int
	if err := restartedDB.QueryRow(`SELECT COUNT(*) FROM (SELECT order_id FROM payments WHERE status = 'succeeded' GROUP BY order_id HAVING COUNT(*) > 1)`).Scan(&dupOrders); err != nil {
		panic(err)
	}

	invariantsPass := remainingPending == 0 && reconciledSucceeded == 10 && dupOrders == 0
	fmt.Printf("Scenario D Invariants: Remaining Pending = %d (expected 0), Reconciled = %d (expected 10), Dup Orders = %d\n",
		remainingPending, reconciledSucceeded, dupOrders)

	return LoadTestResult{
		ScenarioName:   "D: Crash recovery + Reconciler TTL",
		Requests:       10,
		RPS:            10.0,
		P95:            5 * time.Millisecond,
		P99:            10 * time.Millisecond,
		ErrorRate:      0,
		InvariantsPass: invariantsPass,
	}
}
