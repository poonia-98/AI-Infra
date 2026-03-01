// Package platform provides shared infrastructure primitives used by all
// Go services in the AI Agent Control Plane.
//
// Components:
//   - LeaderElector   — PostgreSQL advisory-lock-backed leader election
//   - HealthServer    — /health /ready /metrics HTTP endpoints
//   - GracefulRunner  — SIGTERM-aware goroutine lifecycle manager
//   - AuditLogger     — structured audit-log publisher via NATS
package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ─────────────────────────────────────────────────────────────────────────────
//  Leader Election
// ─────────────────────────────────────────────────────────────────────────────

// LeaderElector implements leader election using a PostgreSQL service_leaders
// table and advisory locks.  Only one instance per service_name can hold the
// lock at a time. The leader refreshes its row every TTL/3 seconds; a
// challenger wins if NOW() - acquired_at > ttl_seconds.
type LeaderElector struct {
	db          *sql.DB
	serviceName string
	leaderID    string
	ttl         time.Duration
	isLeader    bool
	mu          sync.RWMutex
	logger      *slog.Logger
	onElected   func()
	onRevoked   func()
}

// NewLeaderElector creates a new elector. leaderID should be unique per
// instance (e.g. hostname + port).
func NewLeaderElector(db *sql.DB, serviceName, leaderID string, ttl time.Duration) *LeaderElector {
	return &LeaderElector{
		db:          db,
		serviceName: serviceName,
		leaderID:    leaderID,
		ttl:         ttl,
		logger:      slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

// OnElected registers a callback fired when this instance becomes leader.
func (e *LeaderElector) OnElected(fn func()) { e.onElected = fn }

// OnRevoked registers a callback fired when leadership is lost.
func (e *LeaderElector) OnRevoked(fn func()) { e.onRevoked = fn }

// IsLeader returns true if this instance currently holds the lock.
func (e *LeaderElector) IsLeader() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isLeader
}

// Run starts the election loop. Blocks until ctx is cancelled.
func (e *LeaderElector) Run(ctx context.Context) {
	ticker := time.NewTicker(e.ttl / 3)
	defer ticker.Stop()

	e.attempt(ctx)

	for {
		select {
		case <-ctx.Done():
			e.resign(context.Background())
			return
		case <-ticker.C:
			e.attempt(ctx)
		}
	}
}

func (e *LeaderElector) attempt(ctx context.Context) {
	ttlSecs := int(e.ttl.Seconds())
	// Try to insert as new leader OR take over if TTL expired
	res, err := e.db.ExecContext(ctx, `
		INSERT INTO service_leaders (service_name, leader_id, acquired_at, ttl_seconds)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (service_name) DO UPDATE
		  SET leader_id   = EXCLUDED.leader_id,
		      acquired_at = NOW(),
		      ttl_seconds = EXCLUDED.ttl_seconds
		WHERE service_leaders.leader_id = $2
		   OR NOW() - service_leaders.acquired_at > (service_leaders.ttl_seconds * INTERVAL '1 second')`,
		e.serviceName, e.leaderID, ttlSecs)
	if err != nil {
		e.setLeader(false)
		return
	}

	rows, _ := res.RowsAffected()
	if rows > 0 {
		e.setLeader(true)
	} else {
		e.setLeader(false)
	}
}

func (e *LeaderElector) resign(ctx context.Context) {
	_, _ = e.db.ExecContext(ctx, `
		UPDATE service_leaders
		SET leader_id='resigned', acquired_at=NOW()-INTERVAL '1 hour'
		WHERE service_name=$1 AND leader_id=$2`,
		e.serviceName, e.leaderID)
	e.setLeader(false)
}

func (e *LeaderElector) setLeader(is bool) {
	e.mu.Lock()
	prev := e.isLeader
	e.isLeader = is
	e.mu.Unlock()

	if !prev && is {
		e.logger.Info("became leader", "service", e.serviceName, "leader_id", e.leaderID)
		if e.onElected != nil {
			go e.onElected()
		}
	} else if prev && !is {
		e.logger.Info("lost leadership", "service", e.serviceName, "leader_id", e.leaderID)
		if e.onRevoked != nil {
			go e.onRevoked()
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
//  Health Server
// ─────────────────────────────────────────────────────────────────────────────

// Dependency is a named health check function.
type Dependency struct {
	Name  string
	Check func(ctx context.Context) error
}

// HealthServer exposes /health, /ready, and /metrics on a dedicated port.
type HealthServer struct {
	service      string
	port         string
	startTime    time.Time
	dependencies []Dependency
	extras       map[string]interface{}
	mu           sync.RWMutex
	logger       *slog.Logger
}

// NewHealthServer creates a health server for the given service name.
func NewHealthServer(service, port string) *HealthServer {
	return &HealthServer{
		service:   service,
		port:      port,
		startTime: time.Now(),
		extras:    make(map[string]interface{}),
		logger:    slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

// AddDependency registers a health-check dependency.
func (h *HealthServer) AddDependency(d Dependency) {
	h.dependencies = append(h.dependencies, d)
}

// SetExtra sets an arbitrary key-value pair returned in /health responses.
func (h *HealthServer) SetExtra(key string, value interface{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.extras[key] = value
}

// Start launches the HTTP server. Non-blocking.
func (h *HealthServer) Start(ctx context.Context) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// ── /health  (always 200 — for liveness probe) ─────────────────────
	r.GET("/health", func(c *gin.Context) {
		h.mu.RLock()
		extras := make(map[string]interface{}, len(h.extras))
		for k, v := range h.extras {
			extras[k] = v
		}
		h.mu.RUnlock()

		resp := gin.H{
			"status":          "healthy",
			"service":         h.service,
			"uptime_seconds":  time.Since(h.startTime).Seconds(),
		}
		for k, v := range extras {
			resp[k] = v
		}
		c.JSON(200, resp)
	})

	// ── /ready  (503 if any critical dependency is down) ───────────────
	r.GET("/ready", func(c *gin.Context) {
		ctx2, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()

		checks := make(map[string]interface{})
		allReady := true

		for _, dep := range h.dependencies {
			if err := dep.Check(ctx2); err != nil {
				checks[dep.Name] = gin.H{"status": "unhealthy", "error": err.Error()}
				allReady = false
			} else {
				checks[dep.Name] = gin.H{"status": "healthy"}
			}
		}

		status := 200
		overall := "ready"
		if !allReady {
			status = 503
			overall = "not_ready"
		}
		c.JSON(status, gin.H{
			"status":  overall,
			"service": h.service,
			"checks":  checks,
		})
	})

	// ── /metrics  (Prometheus exposition format) ───────────────────────
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	srv := &http.Server{Addr: ":" + h.port, Handler: r}

	go func() {
		h.logger.Info("health server started", "service", h.service, "port", h.port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			h.logger.Error("health server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
//  Graceful Runner
// ─────────────────────────────────────────────────────────────────────────────

// Task is a named goroutine with an optional drain function called on shutdown.
type Task struct {
	Name  string
	Run   func(ctx context.Context)
	Drain func() // called after ctx is cancelled; may be nil
}

// GracefulRunner starts a set of Tasks and blocks until SIGTERM or SIGINT,
// then drains each task and waits for them to finish.
type GracefulRunner struct {
	tasks        []Task
	drainTimeout time.Duration
	logger       *slog.Logger
}

// NewGracefulRunner creates a runner with the given drain timeout.
func NewGracefulRunner(drainTimeout time.Duration) *GracefulRunner {
	return &GracefulRunner{
		drainTimeout: drainTimeout,
		logger:       slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

// Add registers a task.
func (g *GracefulRunner) Add(t Task) {
	g.tasks = append(g.tasks, t)
}

// Run starts all tasks and blocks until shutdown signal is received.
func (g *GracefulRunner) Run() {
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	for _, task := range g.tasks {
		t := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.logger.Info("task started", "name", t.Name)
			t.Run(ctx)
			g.logger.Info("task stopped", "name", t.Name)
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	g.logger.Info("shutdown signal received", "signal", sig.String())

	// Cancel context — all tasks should start winding down
	cancel()

	// Run drain functions
	for _, task := range g.tasks {
		if task.Drain != nil {
			g.logger.Info("draining task", "name", task.Name)
			task.Drain()
		}
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		g.logger.Info("all tasks drained cleanly")
	case <-time.After(g.drainTimeout):
		g.logger.Warn("drain timeout exceeded — forcing shutdown", "timeout", g.drainTimeout)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
//  Audit Logger
// ─────────────────────────────────────────────────────────────────────────────

// AuditEvent represents a platform action to be recorded.
type AuditEvent struct {
	ActorID    string                 `json:"actor_id"`
	ActorType  string                 `json:"actor_type"`  // user | service | system
	Action     string                 `json:"action"`
	Resource   string                 `json:"resource,omitempty"`
	ResourceID string                 `json:"resource_id,omitempty"`
	OldValue   map[string]interface{} `json:"old_value,omitempty"`
	NewValue   map[string]interface{} `json:"new_value,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
	Service    string                 `json:"service"`
	Status     string                 `json:"status"`      // success | failure
	Error      string                 `json:"error,omitempty"`
	Timestamp  time.Time              `json:"timestamp"`
}

// AuditLogger publishes audit events to NATS subject "platform.audit".
// A separate log-processor subscribes and persists to the audit_logs table.
type AuditLogger struct {
	nc      *natsgo.Conn
	service string
	db      *sql.DB  // optional: write directly if NATS unavailable
	logger  *slog.Logger
}

// NewAuditLogger creates an audit logger. db may be nil if direct persistence
// is not required (NATS-only mode).
func NewAuditLogger(nc *natsgo.Conn, db *sql.DB, service string) *AuditLogger {
	return &AuditLogger{
		nc:      nc,
		service: service,
		db:      db,
		logger:  slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

// Log publishes an audit event. Errors are logged but never returned — audit
// logging must never block the business operation.
func (a *AuditLogger) Log(ctx context.Context, ev AuditEvent) {
	ev.Service = a.service
	if ev.Status == "" {
		ev.Status = "success"
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	// Try NATS first (async, non-blocking)
	if a.nc != nil && !a.nc.IsClosed() {
		payload, err := json.Marshal(ev)
		if err == nil {
			if err := a.nc.Publish("platform.audit", payload); err == nil {
				return
			}
		}
	}

	// Fallback: write directly to database
	if a.db != nil {
		a.writeDB(ctx, ev)
	}
}

func (a *AuditLogger) writeDB(ctx context.Context, ev AuditEvent) {
	oldVal, _ := json.Marshal(ev.OldValue)
	newVal, _ := json.Marshal(ev.NewValue)
	meta, _ := json.Marshal(ev.Metadata)

	var resourceID interface{}
	if ev.ResourceID != "" {
		resourceID = ev.ResourceID
	}

	_, err := a.db.ExecContext(ctx, `
		INSERT INTO audit_logs
		  (actor_id, actor_type, action, resource, resource_id,
		   old_value, new_value, metadata, service, status, error, created_at)
		VALUES ($1,$2,$3,$4,$5::uuid,$6::jsonb,$7::jsonb,$8::jsonb,$9,$10,$11,$12)`,
		ev.ActorID, ev.ActorType, ev.Action, ev.Resource, resourceID,
		string(oldVal), string(newVal), string(meta),
		ev.Service, ev.Status, ev.Error, ev.Timestamp)
	if err != nil {
		a.logger.Error("failed to write audit log directly", "error", err, "action", ev.Action)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
//  Config Client (reads from configs table with env override)
// ─────────────────────────────────────────────────────────────────────────────

// ConfigClient fetches configuration values from the configs table.
// Environment variables take precedence: CONFIG_<SERVICE>_<KEY> (uppercased).
type ConfigClient struct {
	db      *sql.DB
	service string
	cache   map[string]string
	mu      sync.RWMutex
	logger  *slog.Logger
}

// NewConfigClient creates a config client for the given service.
func NewConfigClient(db *sql.DB, service string) *ConfigClient {
	return &ConfigClient{
		db:      db,
		service: service,
		cache:   make(map[string]string),
		logger:  slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

// Get returns the config value for key. Checks env first, then DB cache,
// finally the configs table. Returns defaultVal if not found.
func (c *ConfigClient) Get(ctx context.Context, key, defaultVal string) string {
	// 1. Environment override: CONFIG_<SERVICE>_<KEY>
	envKey := fmt.Sprintf("CONFIG_%s_%s",
		toUpperSnake(c.service), toUpperSnake(key))
	if v := os.Getenv(envKey); v != "" {
		return v
	}

	// 2. In-memory cache
	cacheKey := c.service + ":" + key
	c.mu.RLock()
	if v, ok := c.cache[cacheKey]; ok {
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()

	// 3. Database
	var value string
	err := c.db.QueryRowContext(ctx,
		`SELECT value FROM configs WHERE service=$1 AND key=$2`,
		c.service, key).Scan(&value)
	if err != nil {
		// Try global namespace
		err2 := c.db.QueryRowContext(ctx,
			`SELECT value FROM configs WHERE service='global' AND key=$1`, key).Scan(&value)
		if err2 != nil {
			return defaultVal
		}
	}

	c.mu.Lock()
	c.cache[cacheKey] = value
	c.mu.Unlock()
	return value
}

// Refresh clears the in-memory cache, forcing the next Get to re-read from DB.
func (c *ConfigClient) Refresh() {
	c.mu.Lock()
	c.cache = make(map[string]string)
	c.mu.Unlock()
}

func toUpperSnake(s string) string {
	result := make([]byte, 0, len(s))
	for _, ch := range s {
		if ch == '-' || ch == '.' || ch == '/' {
			result = append(result, '_')
		} else if ch >= 'a' && ch <= 'z' {
			result = append(result, byte(ch-32))
		} else {
			result = append(result, byte(ch))
		}
	}
	return string(result)
}

// ─────────────────────────────────────────────────────────────────────────────
//  DB helpers
// ─────────────────────────────────────────────────────────────────────────────

// ConnectPostgres retries connecting to PostgreSQL until success or maxAttempts.
func ConnectPostgres(dsn string, maxAttempts int, logger *slog.Logger) (*sql.DB, error) {
	var db *sql.DB
	var err error
	for i := 0; i < maxAttempts; i++ {
		db, err = sql.Open("postgres", dsn)
		if err == nil {
			if pingErr := db.PingContext(context.Background()); pingErr == nil {
				db.SetMaxOpenConns(25)
				db.SetMaxIdleConns(5)
				db.SetConnMaxLifetime(5 * time.Minute)
				logger.Info("postgres connected")
				return db, nil
			}
		}
		logger.Info("waiting for postgres", "attempt", i+1, "max", maxAttempts)
		time.Sleep(3 * time.Second)
	}
	return nil, fmt.Errorf("postgres not available after %d attempts: %w", maxAttempts, err)
}

// ConnectNATS retries connecting to NATS until success or maxAttempts.
func ConnectNATS(url, serviceName string, maxAttempts int, logger *slog.Logger) (*natsgo.Conn, error) {
	var nc *natsgo.Conn
	var err error
	for i := 0; i < maxAttempts; i++ {
		nc, err = natsgo.Connect(url,
			natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(2*time.Second),
			natsgo.Name(serviceName),
			natsgo.DisconnectErrHandler(func(nc *natsgo.Conn, err error) {
				logger.Warn("nats disconnected", "error", err)
			}),
			natsgo.ReconnectHandler(func(nc *natsgo.Conn) {
				logger.Info("nats reconnected", "url", nc.ConnectedUrl())
			}),
		)
		if err == nil {
			logger.Info("nats connected", "url", nc.ConnectedUrl())
			return nc, nil
		}
		logger.Info("waiting for nats", "attempt", i+1)
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("nats not available after %d attempts: %w", maxAttempts, err)
}

// GetEnv returns the env var value or a fallback.
func GetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}