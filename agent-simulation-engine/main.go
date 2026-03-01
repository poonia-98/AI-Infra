package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	DatabaseURL string
	NatsURL     string
	BackendURL  string
	Port        string
	Workers     int
}

func loadConfig() Config {
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:     getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:  getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		Port:        getEnv("PORT", "8097"),
		Workers:     8,
	}
}

var (
	simRunsStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_sim_runs_started_total",
	}, []string{"run_type"})
	simRunsCompleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_sim_runs_completed_total",
	}, []string{"verdict"})
	activeSimRuns = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_sim_runs_active",
	})
)

func init() {
	prometheus.MustRegister(simRunsStarted, simRunsCompleted, activeSimRuns)
}

type SimConfig struct {
	// Workload config
	RequestsPerSecond float64                `json:"requests_per_second"`
	Duration          int                    `json:"duration_seconds"`
	Concurrency       int                    `json:"concurrency"`
	InputTemplates    []map[string]interface{} `json:"input_templates"`
	// Fault injection
	FaultRate         float64                `json:"fault_rate"`        // 0-1
	FaultTypes        []string               `json:"fault_types"`       // network|cpu|memory|timeout
	NetworkLatencyMs  int                    `json:"network_latency_ms"`
	// Assertions
	Assertions        []Assertion            `json:"assertions"`
}

type Assertion struct {
	Name      string      `json:"name"`
	Type      string      `json:"type"`   // latency | success_rate | output_contains | output_schema
	Operator  string      `json:"operator"` // lt | gt | eq | contains
	Value     interface{} `json:"value"`
}

type SimResult struct {
	TotalRequests    int     `json:"total_requests"`
	SuccessCount     int     `json:"success_count"`
	FailureCount     int     `json:"failure_count"`
	TimeoutCount     int     `json:"timeout_count"`
	SuccessRate      float64 `json:"success_rate"`
	P50LatencyMs     float64 `json:"p50_latency_ms"`
	P90LatencyMs     float64 `json:"p90_latency_ms"`
	P99LatencyMs     float64 `json:"p99_latency_ms"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	MaxLatencyMs     float64 `json:"max_latency_ms"`
	AssertionResults []AssertionResult `json:"assertion_results"`
}

type AssertionResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

type SimulationEngine struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
	jobs   chan string
	wg     sync.WaitGroup
}

func NewSimulationEngine(cfg Config, db *sql.DB, nc *natsgo.Conn) *SimulationEngine {
	return &SimulationEngine{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 60 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		jobs:   make(chan string, 64),
	}
}

func (e *SimulationEngine) StartWorkers(ctx context.Context) {
	for i := 0; i < e.cfg.Workers; i++ {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case runID := <-e.jobs:
					e.executeRun(ctx, runID)
				}
			}
		}()
	}
}

func (e *SimulationEngine) PollPending(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := e.db.QueryContext(ctx, `
				UPDATE simulation_runs
				SET status='running', started_at=NOW()
				WHERE id IN (
					SELECT id FROM simulation_runs WHERE status='queued'
					ORDER BY created_at LIMIT 8 FOR UPDATE SKIP LOCKED
				)
				RETURNING id::text`)
			if err != nil {
				continue
			}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					select {
					case e.jobs <- id:
						simRunsStarted.With(prometheus.Labels{"run_type": "queued"}).Inc()
						activeSimRuns.Inc()
					default:
					}
				}
			}
			rows.Close()
		}
	}
}

func (e *SimulationEngine) executeRun(ctx context.Context, runID string) {
	defer activeSimRuns.Dec()

	// Load run config
	var agentID, runType string
	var configJSON []byte
	err := e.db.QueryRowContext(ctx, `
		SELECT agent_id::text, run_type, config
		FROM simulation_runs WHERE id=$1::uuid`, runID).
		Scan(&agentID, &runType, &configJSON)
	if err != nil {
		e.failRun(runID, "failed to load run: "+err.Error())
		return
	}

	var cfg SimConfig
	json.Unmarshal(configJSON, &cfg)

	// Apply defaults
	if cfg.RequestsPerSecond == 0 {
		cfg.RequestsPerSecond = 10
	}
	if cfg.Duration == 0 {
		cfg.Duration = 30
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 5
	}

	e.logger.Info("simulation run started", "run_id", runID, "agent_id", agentID, "type", runType)

	var result SimResult
	switch runType {
	case "behavior", "performance":
		result = e.runLoadTest(ctx, agentID, cfg)
	case "failure":
		result = e.runFailureTest(ctx, agentID, cfg)
	case "chaos":
		result = e.runChaosTest(ctx, agentID, cfg)
	case "regression":
		result = e.runRegressionTest(ctx, agentID, cfg)
	default:
		result = e.runLoadTest(ctx, agentID, cfg)
	}

	// Evaluate assertions
	result.AssertionResults = e.evaluateAssertions(result, cfg.Assertions)
	passed := 0
	for _, ar := range result.AssertionResults {
		if ar.Passed {
			passed++
		}
	}

	verdict := "pass"
	if passed < len(result.AssertionResults) {
		verdict = "fail"
	}
	if len(result.AssertionResults) == 0 {
		if result.SuccessRate >= 0.95 {
			verdict = "pass"
		} else {
			verdict = "fail"
		}
	}

	resultsJSON, _ := json.Marshal(result)
	assertionsJSON, _ := json.Marshal(result.AssertionResults)
	e.db.ExecContext(ctx, `
		UPDATE simulation_runs
		SET status='completed', results=$1::jsonb, assertions=$2::jsonb,
		    assertions_passed=$3, assertions_total=$4, verdict=$5,
		    completed_at=NOW(),
		    duration_ms=EXTRACT(EPOCH FROM (NOW()-started_at))*1000
		WHERE id=$6::uuid`,
		string(resultsJSON), string(assertionsJSON),
		passed, len(result.AssertionResults), verdict, runID)

	simRunsCompleted.With(prometheus.Labels{"verdict": verdict}).Inc()
	e.logger.Info("simulation run completed",
		"run_id", runID, "verdict", verdict,
		"success_rate", result.SuccessRate,
		"p99_ms", result.P99LatencyMs)
}

func (e *SimulationEngine) runLoadTest(ctx context.Context, agentID string, cfg SimConfig) SimResult {
	type sample struct {
		latency time.Duration
		success bool
	}
	samples := make([]sample, 0, int(cfg.RequestsPerSecond)*cfg.Duration)
	mu := sync.Mutex{}

	deadline := time.Now().Add(time.Duration(cfg.Duration) * time.Second)
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	ticker := time.NewTicker(time.Duration(float64(time.Second) / cfg.RequestsPerSecond))
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			goto done
		case <-ticker.C:
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()

				// Inject fault if configured
				if cfg.FaultRate > 0 && rand.Float64() < cfg.FaultRate {
					mu.Lock()
					samples = append(samples, sample{latency: time.Duration(cfg.NetworkLatencyMs)*time.Millisecond, success: false})
					mu.Unlock()
					return
				}

				input := e.selectInput(cfg.InputTemplates)
				start := time.Now()
				success := e.executeAgent(ctx, agentID, input)
				latency := time.Since(start)

				mu.Lock()
				samples = append(samples, sample{latency: latency, success: success})
				mu.Unlock()
			}()
		}
	}
done:
	wg.Wait()
	return e.computeResult(samples)
}

func (e *SimulationEngine) runFailureTest(ctx context.Context, agentID string, cfg SimConfig) SimResult {
	type sample struct {
		latency time.Duration
		success bool
	}
	var samples []sample

	faultTypes := cfg.FaultTypes
	if len(faultTypes) == 0 {
		faultTypes = []string{"timeout", "network", "cpu"}
	}

	for _, faultType := range faultTypes {
		e.logger.Info("injecting fault", "agent_id", agentID, "fault_type", faultType)

		// Inject fault via NATS
		payload, _ := json.Marshal(map[string]interface{}{
			"agent_id":   agentID,
			"fault_type": faultType,
			"duration":   5,
		})
		e.nc.Publish("simulation.fault.inject", payload)

		// Run a few requests during fault
		for i := 0; i < 5; i++ {
			start := time.Now()
			success := e.executeAgent(ctx, agentID, e.selectInput(cfg.InputTemplates))
			samples = append(samples, sample{latency: time.Since(start), success: success})
			time.Sleep(500 * time.Millisecond)
		}

		// Remove fault
		e.nc.Publish("simulation.fault.remove", payload)
		time.Sleep(2 * time.Second)

		// Run requests after recovery
		for i := 0; i < 5; i++ {
			start := time.Now()
			success := e.executeAgent(ctx, agentID, e.selectInput(cfg.InputTemplates))
			samples = append(samples, sample{latency: time.Since(start), success: success})
			time.Sleep(200 * time.Millisecond)
		}
	}

	return e.computeResult(samples)
}

func (e *SimulationEngine) runChaosTest(ctx context.Context, agentID string, cfg SimConfig) SimResult {
	// Combine random faults with load
	cfg.FaultRate = 0.3
	if len(cfg.FaultTypes) == 0 {
		cfg.FaultTypes = []string{"network", "timeout", "cpu", "memory"}
	}
	return e.runLoadTest(ctx, agentID, cfg)
}

func (e *SimulationEngine) runRegressionTest(ctx context.Context, agentID string, cfg SimConfig) SimResult {
	// Run a standard set of inputs and compare results
	if len(cfg.InputTemplates) == 0 {
		cfg.InputTemplates = []map[string]interface{}{{"type": "regression_default"}}
	}
	cfg.Duration = max(cfg.Duration, 10)
	cfg.RequestsPerSecond = min(cfg.RequestsPerSecond, 5)
	return e.runLoadTest(ctx, agentID, cfg)
}

func (e *SimulationEngine) executeAgent(ctx context.Context, agentID string, input map[string]interface{}) bool {
	payload, _ := json.Marshal(map[string]interface{}{"input": input})
	req, err := http.NewRequestWithContext(ctx, "POST",
		e.cfg.BackendURL+"/api/v1/agents/"+agentID+"/execute",
		bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Simulation", "true")
	resp, err := e.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 400
}

func (e *SimulationEngine) selectInput(templates []map[string]interface{}) map[string]interface{} {
	if len(templates) == 0 {
		return map[string]interface{}{"type": "ping"}
	}
	return templates[rand.Intn(len(templates))]
}

func (e *SimulationEngine) computeResult(samples []struct {
	latency time.Duration
	success bool
}) SimResult {
	if len(samples) == 0 {
		return SimResult{}
	}
	var successCount, failureCount int
	latencies := make([]float64, 0, len(samples))
	var totalLatency float64

	for _, s := range samples {
		ms := float64(s.latency.Milliseconds())
		latencies = append(latencies, ms)
		totalLatency += ms
		if s.success {
			successCount++
		} else {
			failureCount++
		}
	}

	// Sort for percentiles
	sortFloat64s(latencies)
	n := len(latencies)
	p50 := latencies[int(float64(n)*0.50)]
	p90 := latencies[int(float64(n)*0.90)]
	p99 := latencies[int(float64(n)*0.99)]
	maxL := latencies[n-1]

	return SimResult{
		TotalRequests: len(samples),
		SuccessCount:  successCount,
		FailureCount:  failureCount,
		SuccessRate:   float64(successCount) / float64(len(samples)),
		P50LatencyMs:  p50,
		P90LatencyMs:  p90,
		P99LatencyMs:  p99,
		AvgLatencyMs:  totalLatency / float64(n),
		MaxLatencyMs:  maxL,
	}
}

func (e *SimulationEngine) evaluateAssertions(result SimResult, assertions []Assertion) []AssertionResult {
	var results []AssertionResult
	for _, a := range assertions {
		ar := AssertionResult{Name: a.Name}
		switch a.Type {
		case "success_rate":
			target, _ := toFloat(a.Value)
			switch a.Operator {
			case "gt", "gte":
				ar.Passed = result.SuccessRate >= target
			case "lt":
				ar.Passed = result.SuccessRate < target
			}
			ar.Message = fmt.Sprintf("success_rate=%.3f %s %.3f", result.SuccessRate, a.Operator, target)
		case "latency":
			target, _ := toFloat(a.Value)
			switch a.Operator {
			case "lt":
				ar.Passed = result.P99LatencyMs < target
				ar.Message = fmt.Sprintf("p99_latency=%.0fms < %.0fms", result.P99LatencyMs, target)
			case "gt":
				ar.Passed = result.P99LatencyMs > target
			}
		}
		results = append(results, ar)
	}
	return results
}

func (e *SimulationEngine) failRun(runID, errMsg string) {
	e.db.Exec(`UPDATE simulation_runs SET status='failed', error=$1, completed_at=NOW() WHERE id=$2::uuid`,
		errMsg, runID)
}

func (e *SimulationEngine) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "agent-simulation-engine"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := e.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Create simulation environment
	r.POST("/environments", func(c *gin.Context) {
		var body struct {
			OrgID       string                 `json:"organisation_id" binding:"required"`
			Name        string                 `json:"name" binding:"required"`
			Description string                 `json:"description"`
			EnvType     string                 `json:"environment_type"`
			WorkloadCfg map[string]interface{} `json:"workload_config"`
			FaultCfg    map[string]interface{} `json:"fault_config"`
			CreatedBy   string                 `json:"created_by"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.EnvType == "" {
			body.EnvType = "isolated"
		}
		wcJSON, _ := json.Marshal(body.WorkloadCfg)
		fcJSON, _ := json.Marshal(body.FaultCfg)
		var id string
		err := e.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO simulation_environments
			  (organisation_id, name, description, environment_type,
			   workload_config, fault_config, created_by)
			VALUES ($1::uuid,$2,$3,$4,$5::jsonb,$6::jsonb,$7)
			RETURNING id::text`,
			body.OrgID, body.Name, body.Description, body.EnvType,
			string(wcJSON), string(fcJSON), body.CreatedBy).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id, "name": body.Name})
	})

	// Queue a simulation run
	r.POST("/runs", func(c *gin.Context) {
		var body struct {
			EnvID   string    `json:"environment_id" binding:"required"`
			AgentID string    `json:"agent_id" binding:"required"`
			OrgID   string    `json:"organisation_id"`
			RunType string    `json:"run_type"`
			Config  SimConfig `json:"config"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.RunType == "" {
			body.RunType = "behavior"
		}
		cfgJSON, _ := json.Marshal(body.Config)
		var id string
		err := e.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO simulation_runs
			  (environment_id, agent_id, organisation_id, run_type, config, status)
			VALUES ($1::uuid,$2::uuid,NULLIF($3,'')::uuid,$4,$5::jsonb,'queued')
			RETURNING id::text`,
			body.EnvID, body.AgentID, body.OrgID, body.RunType, string(cfgJSON)).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		simRunsStarted.With(prometheus.Labels{"run_type": body.RunType}).Inc()
		c.JSON(202, gin.H{"run_id": id, "status": "queued"})
	})

	// Get run results
	r.GET("/runs/:id", func(c *gin.Context) {
		row := e.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text, agent_id::text, run_type, status,
			       results, assertions, assertions_passed, assertions_total,
			       verdict, error, duration_ms, created_at, completed_at
			FROM simulation_runs WHERE id=$1::uuid`, c.Param("id"))
		var id, agentID, runType, status, verdict, errMsg string
		var assertPassed, assertTotal int
		var durationMs *int64
		var results, assertionsJSON []byte
		var createdAt time.Time
		var completedAt *time.Time
		if err := row.Scan(&id, &agentID, &runType, &status, &results,
			&assertionsJSON, &assertPassed, &assertTotal, &verdict, &errMsg,
			&durationMs, &createdAt, &completedAt); err != nil {
			c.JSON(404, gin.H{"error": "not found"})
			return
		}
		var r, a interface{}
		json.Unmarshal(results, &r)
		json.Unmarshal(assertionsJSON, &a)
		c.JSON(200, gin.H{
			"id": id, "agent_id": agentID, "run_type": runType,
			"status": status, "results": r, "assertions": a,
			"assertions_passed": assertPassed, "assertions_total": assertTotal,
			"verdict": verdict, "error": errMsg, "duration_ms": durationMs,
			"created_at": createdAt, "completed_at": completedAt,
		})
	})

	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting agent-simulation-engine", "port", cfg.Port)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil && db.PingContext(context.Background()) == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		logger.Error("postgres unavailable", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(15)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.Name("agent-simulation-engine"))
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Error("nats unavailable", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

	engine := NewSimulationEngine(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.StartWorkers(ctx)
	go engine.PollPending(ctx)

	router := engine.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	cancel()
	engine.wg.Wait()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func sortFloat64s(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}