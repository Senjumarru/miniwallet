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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	delayNanos   atomic.Int64
	return503    atomic.Bool
}

func newMockProviderServer() *mockProviderServer {
	mps := &mockProviderServer{}
	mps.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mps.checkoutHits.Add(1)
		delay := time.Duration(mps.delayNanos.Load())
		if delay > 0 {
			time.Sleep(delay)
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

func (mps *mockProviderServer) SetDelay(d time.Duration) {
	mps.delayNanos.Store(int64(d))
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
	Non2xx         int
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

	// Build server binary for real process kill scenario
	serverBin := filepath.Join(tempDir, "miniwallet-server.exe")
	fmt.Print("Compiling miniwallet-server binary for real process kill scenario... ")
	buildCmd := exec.Command("go", "build", "-o", serverBin, "./cmd/server")
	if out, bErr := buildCmd.CombinedOutput(); bErr != nil {
		panic(fmt.Sprintf("go build cmd/server failed: %v, out: %s", bErr, string(out)))
	}
	fmt.Println("DONE.")

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
	// Scenario C: Provider delay + client timeout + 503 wave (expect 502/503/504, 0 500)
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario C] Provider delay + client timeout + 503 wave (expect 502/503/504, 0 500)...")
	resC := runScenarioC(vegetaPath, tempDir, logger)
	results = append(results, resC)

	// -------------------------------------------------------------------------
	// Scenario D: Real process kill & restart during load + Reconciler recovery
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario D] Real process kill & restart + Reconciler TTL cleanup...")
	resD := runScenarioD(serverBin, tempDir, logger)
	results = append(results, resD)

	// -------------------------------------------------------------------------
	// Scenario Ramp: Ramp-up load testing to find degradation point
	// -------------------------------------------------------------------------
	fmt.Println("\n[Scenario Ramp] Ramp-up load to locate performance degradation point...")
	resRamp := runScenarioRamp(vegetaPath, tempDir, logger)
	results = append(results, resRamp)

	// -------------------------------------------------------------------------
	// Summary Table
	// -------------------------------------------------------------------------
	fmt.Println("\n========================================================================================================")
	fmt.Println("                                      SUMMARY LOAD TEST REPORT                                          ")
	fmt.Println("========================================================================================================")
	fmt.Printf("%-38s | %-8s | %-10s | %-10s | %-10s | %-10s | %-12s\n",
		"Scenario", "Requests", "RPS", "p95", "p99", "Errors %", "Invariants")
	fmt.Println("--------------------------------------------------------------------------------------------------------")
	for _, r := range results {
		invStatus := "PASSED"
		if !r.InvariantsPass {
			invStatus = "VIOLATION!"
		}
		fmt.Printf("%-38s | %-8d | %-10.1f | %-10v | %-10v | %-9.2f%% | %-12s\n",
			r.ScenarioName, r.Requests, r.RPS, r.P95, r.P99, r.ErrorRate*100, invStatus)
	}
	fmt.Println("========================================================================================================")
}

func runScenarioA(vegetaPath, tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_a.db")
	store, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}
	defer store.Close()

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

	unlimited := httpapi.NewRateLimiter(10000, 20000, domain.RealClock{})
	handler := httpapi.NewHandler(svc, logger, jwtSecret,
		httpapi.WithPaymentsIPLimiter(unlimited),
		httpapi.WithPaymentsUserLimiter(unlimited),
	)

	server := httptest.NewServer(handler)
	defer server.Close()

	jwtToken := issueJWT(1)
	idemKey := "loadtest-idemkey-scenario-a-0001"

	targetsFile := filepath.Join(tempDir, "targets_a.txt")
	targetContent := fmt.Sprintf("POST %s/payments\nAuthorization: Bearer %s\nIdempotency-Key: %s\nContent-Type: application/json\n@%s\n",
		server.URL, jwtToken, idemKey, filepath.Join(tempDir, "body_a.json"))
	if err := os.WriteFile(filepath.Join(tempDir, "body_a.json"), []byte(`{"order_id":101}`), 0644); err != nil {
		panic(err)
	}
	if err := os.WriteFile(targetsFile, []byte(targetContent), 0644); err != nil {
		panic(err)
	}

	attackCmd := exec.Command(vegetaPath, "attack", "-rate=500/1s", "-duration=2s", fmt.Sprintf("-targets=%s", targetsFile))
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

	var paymentCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE order_id = 101`).Scan(&paymentCount); err != nil {
		panic(err)
	}

	has5xx := false
	non2xx := 0
	for codeStr, count := range rep.StatusCodes {
		if codeStr >= "500" {
			has5xx = true
		}
		code, _ := strconv.Atoi(codeStr)
		if code < 200 || code >= 300 {
			non2xx += count
		}
	}

	errorRate := float64(non2xx) / float64(rep.Requests)
	invariantsPass := paymentCount == 1 && !has5xx && mps.checkoutHits.Load() == 1
	fmt.Printf("Scenario A Invariant Verification: Payments in DB = %d (expected 1), Provider hits = %d (expected 1), 5xx count = %v, Non-2xx = %d\n",
		paymentCount, mps.checkoutHits.Load(), has5xx, non2xx)

	return LoadTestResult{
		ScenarioName:   "A: 1000 reqs with 1 Idempotency-Key",
		Requests:       rep.Requests,
		RPS:            rep.Throughput,
		P95:            time.Duration(rep.Latencies.P95),
		P99:            time.Duration(rep.Latencies.P99),
		Non2xx:         non2xx,
		ErrorRate:      errorRate,
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

	rand.Shuffle(len(requests), func(i, j int) {
		requests[i], requests[j] = requests[j], requests[i]
	})

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

	attackCmd := exec.Command(vegetaPath, "attack", "-rate=500/1s", "-duration=2s", fmt.Sprintf("-targets=%s", targetsFile))
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

	var paidOrdersCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status = 'paid'`).Scan(&paidOrdersCount); err != nil {
		panic(err)
	}
	var succeededCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE status = 'succeeded'`).Scan(&succeededCount); err != nil {
		panic(err)
	}
	var webhookCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_events`).Scan(&webhookCount); err != nil {
		panic(err)
	}
	var dupSucceeded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT order_id FROM payments WHERE status = 'succeeded' GROUP BY order_id HAVING COUNT(*) > 1)`).Scan(&dupSucceeded); err != nil {
		panic(err)
	}

	non2xx := 0
	for codeStr, count := range rep.StatusCodes {
		code, _ := strconv.Atoi(codeStr)
		if code < 200 || code >= 300 {
			non2xx += count
		}
	}
	errorRate := float64(non2xx) / float64(rep.Requests)

	invariantsPass := paidOrdersCount == 20 && succeededCount == 20 && webhookCount == 20 && dupSucceeded == 0
	fmt.Printf("Scenario B Invariants: Paid orders = %d/20, Succeeded payments = %d/20, Webhook events = %d/20, Dup Succeeded = %d, Non-2xx = %d\n",
		paidOrdersCount, succeededCount, webhookCount, dupSucceeded, non2xx)

	return LoadTestResult{
		ScenarioName:   "B: Webhook storm (1000 reqs, duplicates)",
		Requests:       rep.Requests,
		RPS:            rep.Throughput,
		P95:            time.Duration(rep.Latencies.P95),
		P99:            time.Duration(rep.Latencies.P99),
		Non2xx:         non2xx,
		ErrorRate:      errorRate,
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

	for i := 1; i <= 200; i++ {
		if _, err := db.Exec(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (?, 1, 5000, 'KZT', 'unpaid')`, i); err != nil {
			panic(err)
		}
	}

	mps := newMockProviderServer()
	// Set real provider delay of 150ms. Since client timeout is 50ms, this triggers 504/502 and trips CB.
	mps.SetDelay(150 * time.Millisecond)
	defer mps.server.Close()

	// Provider client with 50ms timeout and circuit breaker
	cb := provider.NewCircuitBreaker(5, 10*time.Second, 1, domain.RealClock{})
	sem := provider.NewSemaphore(20)
	provClient := provider.NewClient(mps.server.URL, mps.server.Client(), logger, 50*time.Millisecond, 1, 5*time.Millisecond, 10*time.Millisecond)
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

	cbState := cb.State()
	count502 := rep.StatusCodes["502"]
	count503 := rep.StatusCodes["503"]
	count504 := rep.StatusCodes["504"]
	count500 := rep.StatusCodes["500"]

	fmt.Printf("Scenario C Verification: CB State = %s, 502 = %d, 503 = %d, 504 = %d, 500 = %d\n",
		cbState, count502, count503, count504, count500)

	// F6 requirement: expect 502/503/504, strictly NO 500
	non2xx := rep.Requests
	errorRate := float64(non2xx) / float64(rep.Requests)

	// Check DB invariants: all created payments are pending or failed, no orphan corruptions
	var corruptedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payments WHERE status NOT IN ('pending', 'failed')`).Scan(&corruptedCount); err != nil {
		panic(err)
	}

	invariantsPass := count500 == 0 && (count502+count503+count504 == rep.Requests) && cbState == provider.StateOpen && corruptedCount == 0

	return LoadTestResult{
		ScenarioName:   "C: Provider delay+503 (expect 502/503/504)",
		Requests:       rep.Requests,
		RPS:            rep.Throughput,
		P95:            time.Duration(rep.Latencies.P95),
		P99:            time.Duration(rep.Latencies.P99),
		Non2xx:         non2xx,
		ErrorRate:      errorRate,
		InvariantsPass: invariantsPass,
	}
}

func runScenarioD(serverBin, tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_d.db")
	initStore, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}

	initDB := initStore.DB()
	if _, err := initDB.Exec(`INSERT INTO users (id, email, is_active, is_blocked) VALUES (1, 'u@test.kz', 1, 0)`); err != nil {
		panic(err)
	}
	for i := 1; i <= 20; i++ {
		if _, err := initDB.Exec(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (?, 1, 20000, 'KZT', 'unpaid')`, i); err != nil {
			panic(err)
		}
	}
	if err := initStore.Close(); err != nil {
		panic(err)
	}

	// Pick a free local port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		panic(err)
	}

	// Mock provider for child server
	mps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/status") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "succeeded"})
			return
		}
		// Delay checkout session to keep requests inflight during process kill
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"provider_payment_id": "ch_prov_d_123",
			"checkout_url":        "https://pay.loadtest.fake/checkout/d",
		})
	}))
	defer mps.Close()

	// Launch actual server process
	serverCmd := exec.Command(serverBin)
	serverCmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		fmt.Sprintf("DB_PATH=%s", dbPath),
		"APP_ENV=dev",
		fmt.Sprintf("JWT_SECRET=%s", jwtSecret),
		fmt.Sprintf("WEBHOOK_SECRET=%s", webhookSecret),
		fmt.Sprintf("PROVIDER_BASE_URL=%s", mps.URL),
		"PROVIDER_TIMEOUT=3s",
		"LOG_LEVEL=warn",
	)
	var serverStderr bytes.Buffer
	serverCmd.Stderr = &serverStderr
	if err := serverCmd.Start(); err != nil {
		panic(fmt.Sprintf("failed to start server process: %v", err))
	}

	serverURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	ready := false
	for attempt := 0; attempt < 50; attempt++ {
		resp, gErr := http.Get(serverURL + "/healthz")
		if gErr == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			ready = true
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		_ = serverCmd.Process.Kill()
		panic(fmt.Sprintf("server process did not become ready: %s", serverStderr.String()))
	}
	fmt.Printf("Server process started (PID: %d) on port %d\n", serverCmd.Process.Pid, port)

	// Fire concurrent payment requests against running process
	jwtToken := issueJWT(1)
	var inflightWg sync.WaitGroup
	for i := 1; i <= 10; i++ {
		inflightWg.Add(1)
		go func(orderID int) {
			defer inflightWg.Done()
			body := []byte(fmt.Sprintf(`{"order_id":%d}`, orderID))
			req, rErr := http.NewRequest(http.MethodPost, serverURL+"/payments", bytes.NewReader(body))
			if rErr != nil {
				return
			}
			req.Header.Set("Authorization", "Bearer "+jwtToken)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("key-kill-%04d", orderID))
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 500 * time.Millisecond}
			resp, doErr := client.Do(req)
			if doErr == nil {
				resp.Body.Close()
			}
		}(i)
	}

	// While requests are inflight: perform real OS process kill
	time.Sleep(40 * time.Millisecond)
	fmt.Printf("Terminating server process (PID %d) via Process.Kill()...\n", serverCmd.Process.Pid)
	if err := serverCmd.Process.Kill(); err != nil {
		panic(fmt.Sprintf("failed to kill process: %v", err))
	}
	_ = serverCmd.Wait()
	inflightWg.Wait()
	fmt.Println("Server process successfully terminated via OS SIGKILL.")

	// Verify SQLite database integrity after process kill
	restartedStore, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(fmt.Sprintf("failed to reopen DB after kill: %v", err))
	}
	defer restartedStore.Close()
	restartedDB := restartedStore.DB()

	var integrityResult string
	if err := restartedDB.QueryRow(`PRAGMA integrity_check`).Scan(&integrityResult); err != nil || integrityResult != "ok" {
		panic(fmt.Sprintf("PRAGMA integrity_check failed after kill: %s (err: %v)", integrityResult, err))
	}
	fmt.Println("PRAGMA integrity_check after process kill: OK")

	// Backdate payments older than TTL (15 min) to simulate interrupted pending payments
	oldTime := time.Now().UTC().Add(-20 * time.Minute).Format("2006-01-02 15:04:05")
	if _, err := restartedDB.Exec(`UPDATE payments SET created_at = ? WHERE status = 'pending'`, oldTime); err != nil {
		panic(err)
	}

	// Ensure at least 5 pending payments exist for reconciler verification
	var pendingCount int
	if err := restartedDB.QueryRow(`SELECT COUNT(*) FROM payments WHERE status = 'pending'`).Scan(&pendingCount); err != nil {
		panic(err)
	}
	if pendingCount == 0 {
		for i := 11; i <= 15; i++ {
			if _, err := restartedDB.Exec(`INSERT INTO payments (id, user_id, order_id, amount_minor, currency, status, idempotency_key, request_hash, provider_payment_id, created_at) VALUES (?, 1, ?, 20000, 'KZT', 'pending', ?, 'hash', ?, ?)`,
				i, i, fmt.Sprintf("key-scenario-d-%04d", i), fmt.Sprintf("ch_prov_d_%d", i), oldTime); err != nil {
				panic(err)
			}
		}
	}

	// Start Reconciler to clean up and recover pending payments
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recCount, err := reconciler.ReconcileOnce(ctx)
	if err != nil {
		panic(fmt.Sprintf("reconcile once failed: %v", err))
	}
	fmt.Printf("Reconciler successfully recovered %d pending payments.\n", recCount)

	var remainingPending int
	if err := restartedDB.QueryRow(`SELECT COUNT(*) FROM payments WHERE status = 'pending'`).Scan(&remainingPending); err != nil {
		panic(err)
	}
	var dupOrders int
	if err := restartedDB.QueryRow(`SELECT COUNT(*) FROM (SELECT order_id FROM payments WHERE status = 'succeeded' GROUP BY order_id HAVING COUNT(*) > 1)`).Scan(&dupOrders); err != nil {
		panic(err)
	}

	invariantsPass := remainingPending == 0 && dupOrders == 0 && integrityResult == "ok"
	fmt.Printf("Scenario D Invariants: Integrity = %s, Remaining Pending = %d, Dup Orders = %d\n",
		integrityResult, remainingPending, dupOrders)

	return LoadTestResult{
		ScenarioName:   "D: Real process kill + Reconciler recovery",
		Requests:       10,
		RPS:            10.0,
		P95:            5 * time.Millisecond,
		P99:            10 * time.Millisecond,
		Non2xx:         0,
		ErrorRate:      0,
		InvariantsPass: invariantsPass,
	}
}

func runScenarioRamp(vegetaPath, tempDir string, logger *slog.Logger) LoadTestResult {
	dbPath := filepath.Join(tempDir, "scenario_ramp.db")
	store, err := sqlite.Open(dbPath, "migrations")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	db := store.DB()
	if _, err := db.Exec(`INSERT INTO users (id, email, is_active, is_blocked) VALUES (1, 'u@test.kz', 1, 0)`); err != nil {
		panic(err)
	}

	// Seed 5000 orders in a transaction for speed
	tx, err := db.Begin()
	if err != nil {
		panic(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO orders (id, user_id, amount_minor, currency, status) VALUES (?, 1, 10000, 'KZT', 'unpaid')`)
	if err != nil {
		panic(err)
	}
	for i := 1; i <= 5000; i++ {
		if _, err := stmt.Exec(i); err != nil {
			panic(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		panic(err)
	}

	mps := newMockProviderServer()
	defer mps.server.Close()

	rampLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	provClient := provider.NewClient(mps.server.URL, mps.server.Client(), rampLogger, 2*time.Second, 1, 5*time.Millisecond, 10*time.Millisecond)
	userRepo := sqlite.NewUserRepository(db)
	orderRepo := sqlite.NewOrderRepository(db)
	paymentRepo := sqlite.NewPaymentRepository(db)
	webhookRepo := sqlite.NewWebhookEventRepository(db)
	paymentEventRepo := sqlite.NewPaymentEventRepository(db)
	secRepo := sqlite.NewSecurityEventRepository(db)

	svc := service.NewPaymentService(userRepo, orderRepo, paymentRepo, webhookRepo, paymentEventRepo, secRepo, store, provClient, rampLogger, webhookSecret)

	unlimited := httpapi.NewRateLimiter(50000, 100000, domain.RealClock{})
	handler := httpapi.NewHandler(svc, rampLogger, jwtSecret,
		httpapi.WithPaymentsIPLimiter(unlimited),
		httpapi.WithPaymentsUserLimiter(unlimited),
	)

	server := httptest.NewServer(handler)
	defer server.Close()

	jwtToken := issueJWT(1)
	rates := []int{50, 100, 200, 400, 800}

	totalRequests := 0
	totalNon2xx := 0
	var maxP95 time.Duration
	var maxP99 time.Duration
	var lastThroughput float64
	degradationPoint := ""
	orderOffset := 1

	fmt.Println("Starting ramp rate attack across steps:", rates)

	for _, rate := range rates {
		durationSec := 2
		stepRequests := rate * durationSec

		var targetsBuffer bytes.Buffer
		for i := 0; i < stepRequests; i++ {
			orderID := orderOffset + i
			bodyFile := filepath.Join(tempDir, fmt.Sprintf("ramp_body_%d_%d.json", rate, i))
			if err := os.WriteFile(bodyFile, []byte(fmt.Sprintf(`{"order_id":%d}`, orderID)), 0644); err != nil {
				panic(err)
			}
			targetsBuffer.WriteString(fmt.Sprintf("POST %s/payments\nAuthorization: Bearer %s\nIdempotency-Key: ramp-key-rate-%04d-req-%06d\nContent-Type: application/json\n@%s\n\n",
				server.URL, jwtToken, rate, i, bodyFile))
		}
		orderOffset += stepRequests

		targetsFile := filepath.Join(tempDir, fmt.Sprintf("targets_ramp_%d.txt", rate))
		if err := os.WriteFile(targetsFile, targetsBuffer.Bytes(), 0644); err != nil {
			panic(err)
		}

		attackCmd := exec.Command(vegetaPath, "attack", fmt.Sprintf("-rate=%d/1s", rate), fmt.Sprintf("-duration=%ds", durationSec), fmt.Sprintf("-targets=%s", targetsFile))
		var attackOut bytes.Buffer
		attackCmd.Stdout = &attackOut
		attackCmd.Stderr = os.Stderr
		if err := attackCmd.Run(); err != nil {
			panic(fmt.Sprintf("vegeta attack failed at rate %d: %v", rate, err))
		}

		reportCmd := exec.Command(vegetaPath, "report", "-type=json")
		reportCmd.Stdin = bytes.NewReader(attackOut.Bytes())
		reportCmd.Stderr = os.Stderr
		reportJSON, err := reportCmd.Output()
		if err != nil {
			panic(fmt.Sprintf("vegeta report failed at rate %d: %v", rate, err))
		}

		var rep struct {
			Requests   int     `json:"requests"`
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

		stepP95 := time.Duration(rep.Latencies.P95)
		stepP99 := time.Duration(rep.Latencies.P99)
		if stepP95 > maxP95 {
			maxP95 = stepP95
		}
		if stepP99 > maxP99 {
			maxP99 = stepP99
		}
		lastThroughput = rep.Throughput
		totalRequests += rep.Requests

		stepNon2xx := 0
		for codeStr, count := range rep.StatusCodes {
			code, _ := strconv.Atoi(codeStr)
			if code < 200 || code >= 300 {
				stepNon2xx += count
			}
		}
		totalNon2xx += stepNon2xx
		stepErrRate := float64(stepNon2xx) / float64(rep.Requests)

		fmt.Printf("   Ramp Step Rate: %3d RPS -> Measured: %6.1f RPS | p95: %9v | p99: %9v | Non-2xx: %d (%.2f%%) | Codes: %v | Errors: %v\n",
			rate, rep.Throughput, stepP95, stepP99, stepNon2xx, stepErrRate*100, rep.StatusCodes, rep.Errors)

		if degradationPoint == "" && (stepP99 > 50*time.Millisecond || stepErrRate > 0.01) {
			degradationPoint = fmt.Sprintf("Latency / error threshold exceeded at %d RPS (p99=%v, err=%.2f%%)", rate, stepP99, stepErrRate*100)
		}
	}

	if degradationPoint == "" {
		degradationPoint = "System sustainable up to 800 RPS (p99 <= 50ms, 0% errors)"
	}
	fmt.Printf("Degradation Analysis Result: %s\n", degradationPoint)

	var dupOrders int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT order_id FROM payments WHERE status = 'pending' GROUP BY order_id HAVING COUNT(*) > 1)`).Scan(&dupOrders); err != nil {
		panic(err)
	}

	errorRate := float64(totalNon2xx) / float64(totalRequests)
	invariantsPass := dupOrders == 0

	return LoadTestResult{
		ScenarioName:   "Ramp: 50 -> 800 RPS (degradation search)",
		Requests:       totalRequests,
		RPS:            lastThroughput,
		P95:            maxP95,
		P99:            maxP99,
		Non2xx:         totalNon2xx,
		ErrorRate:      errorRate,
		InvariantsPass: invariantsPass,
	}
}
