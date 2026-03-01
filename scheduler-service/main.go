package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/robfig/cron/v3"
)

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL   string
	NatsURL       string
	BackendURL    string
	ControlPlaneAPIKey string
	TickInterval  time.Duration
	Port          string
}

func loadConfig() Config {
	tick, _ := time.ParseDuration(getEnv("TICK_INTERVAL", "10s"))
	return Config{
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:      getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:   getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		ControlPlaneAPIKey: getEnv("CONTROL_PLANE_API_KEY", ""),
		TickInterval: tick,
		Port:         getEnv("PORT", "8086"),
	}
}

// ── Domain ────────────────────────────────────────────────────────────────────

type Schedule struct {
	ID              string
	AgentID         string
	ScheduleType    string // cron | once | interval
	CronExpr        *string
	IntervalSeconds *int
	RunAt           *time.Time
	Enabled         bool
	Timezone        string
	Input           json.RawMessage
	LastRunAt       *time.Time
	NextRunAt       *time.Time
	RunCount        int
	MaxRuns         *int
}

// ── Scheduler ────────────────────────────────────────────────────────────────

type Scheduler struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
	http   *http.Client
	cronP  *cron.Parser
}

func NewScheduler(cfg Config, db *sql.DB, nc *natsgo.Conn) *Scheduler {
	p := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &Scheduler{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		http:   &http.Client{Timeout: 30 * time.Second},
		cronP:  &p,
	}
}

// ── Main tick loop ────────────────────────────────────────────────────────────

func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("scheduler started", "tick_interval", s.cfg.TickInterval)

	// ── Leader election ────────────────────────────────────────────────
	hostname, _ := os.Hostname()
	leaderID := "scheduler@" + hostname
	var isLeader bool
	ttl := 30
	electTicker := time.NewTicker(time.Duration(ttl/3) * time.Second)
	defer electTicker.Stop()

	tryElect := func() {
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO service_leaders (service_name, leader_id, acquired_at, ttl_seconds)
			VALUES ('scheduler', $1, NOW(), $2)
			ON CONFLICT (service_name) DO UPDATE
			  SET leader_id=$1, acquired_at=NOW(), ttl_seconds=$2
			WHERE service_leaders.leader_id=$1
			   OR NOW()-service_leaders.acquired_at > (service_leaders.ttl_seconds*INTERVAL '1 second')`,
			leaderID, ttl)
		if err == nil {
			rows, _ := res.RowsAffected()
			prev := isLeader
			isLeader = rows > 0
			if !prev && isLeader {
				s.logger.Info("became scheduler leader", "leader_id", leaderID)
			}
		} else {
			isLeader = true // fallback if service_leaders table doesn't exist yet
		}
	}
	tryElect()

	// Compute next_run_at for all schedules that don't have it yet
	s.initNextRunAt(ctx)

	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()

	if isLeader {
		s.tick(ctx) // immediate first tick
	}

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopped")
			return
		case <-electTicker.C:
			tryElect()
		case <-ticker.C:
			if isLeader {
				s.tick(ctx)
			}
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	schedules, err := s.fetchDueSchedules(ctx, now)
	if err != nil {
		s.logger.Error("failed to fetch due schedules", "error", err)
		return
	}

	for _, sched := range schedules {
		s.execute(ctx, sched, now)
	}
}

// ── Schedule fetching ─────────────────────────────────────────────────────────

func (s *Scheduler) fetchDueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, agent_id::text, schedule_type,
		        cron_expr, interval_seconds, run_at,
		        enabled, timezone, COALESCE(input,'{}'),
		        last_run_at, next_run_at,
		        run_count, max_runs
		 FROM agent_schedules
		 WHERE enabled = true
		   AND (next_run_at IS NULL OR next_run_at <= $1)
		   AND (max_runs IS NULL OR run_count < max_runs)
		 FOR UPDATE SKIP LOCKED`,
		now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		var sched Schedule
		if err := rows.Scan(
			&sched.ID, &sched.AgentID, &sched.ScheduleType,
			&sched.CronExpr, &sched.IntervalSeconds, &sched.RunAt,
			&sched.Enabled, &sched.Timezone, &sched.Input,
			&sched.LastRunAt, &sched.NextRunAt,
			&sched.RunCount, &sched.MaxRuns,
		); err != nil {
			return nil, err
		}
		out = append(out, sched)
	}
	return out, rows.Err()
}

// ── Execution trigger ─────────────────────────────────────────────────────────

func (s *Scheduler) execute(ctx context.Context, sched Schedule, now time.Time) {
	s.logger.Info("triggering scheduled execution",
		"schedule_id", sched.ID, "agent_id", sched.AgentID, "type", sched.ScheduleType)

	// Compute next run BEFORE incrementing count
	nextRun := s.computeNextRun(sched, now)

	// Mark as started (idempotent — update only if next_run_at unchanged)
	result, err := s.db.ExecContext(ctx,
		`UPDATE agent_schedules
		 SET last_run_at=$1, run_count=run_count+1, next_run_at=$2,
		     enabled = CASE WHEN max_runs IS NOT NULL AND run_count+1 >= max_runs THEN false ELSE enabled END
		 WHERE id=$3 AND (next_run_at IS NULL OR next_run_at <= $1)`,
		now, nextRun, sched.ID)
	if err != nil {
		s.logger.Error("failed to update schedule", "error", err, "schedule_id", sched.ID)
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return // Another instance got it first (SKIP LOCKED handles this, but be safe)
	}

	// Trigger the agent start via backend API
	go s.triggerAgentStart(ctx, sched)
}

func (s *Scheduler) triggerAgentStart(ctx context.Context, sched Schedule) {
	url := fmt.Sprintf("%s/api/v1/agents/%s/start", s.cfg.BackendURL, sched.AgentID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(sched.Input))
	if err != nil {
		s.logger.Error("failed to build start request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scheduler-ID", sched.ID)
	if s.cfg.ControlPlaneAPIKey != "" {
		req.Header.Set("X-Service-API-Key", s.cfg.ControlPlaneAPIKey)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		s.logger.Error("failed to trigger agent start", "error", err, "agent_id", sched.AgentID)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.logger.Info("scheduled execution triggered", "agent_id", sched.AgentID, "status", resp.StatusCode)
		s.publishEvent("agent.scheduled.triggered", map[string]interface{}{
			"schedule_id": sched.ID, "agent_id": sched.AgentID,
			"schedule_type": sched.ScheduleType, "timestamp": time.Now().UTC(),
		})
	} else {
		s.logger.Warn("agent start returned non-2xx", "status", resp.StatusCode, "agent_id", sched.AgentID)
	}
}

// ── Next run computation ──────────────────────────────────────────────────────

func (s *Scheduler) computeNextRun(sched Schedule, after time.Time) *time.Time {
	switch sched.ScheduleType {
	case "cron":
		if sched.CronExpr == nil {
			return nil
		}
		sched2, err := s.cronP.Parse(*sched.CronExpr)
		if err != nil {
			s.logger.Error("invalid cron expression", "expr", *sched.CronExpr, "error", err)
			return nil
		}
		next := sched2.Next(after)
		return &next

	case "interval":
		if sched.IntervalSeconds == nil {
			return nil
		}
		next := after.Add(time.Duration(*sched.IntervalSeconds) * time.Second)
		return &next

	case "once":
		// Disable after first run
		_, _ = s.db.ExecContext(context.Background(),
			`UPDATE agent_schedules SET enabled=false WHERE id=$1`, sched.ID)
		return nil
	}
	return nil
}

func (s *Scheduler) initNextRunAt(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, schedule_type, cron_expr, interval_seconds, run_at
		 FROM agent_schedules
		 WHERE enabled=true AND next_run_at IS NULL`)
	if err != nil {
		return
	}
	defer rows.Close()

	now := time.Now().UTC()
	for rows.Next() {
		var id, schedType string
		var cronExpr *string
		var intervalSec *int
		var runAt *time.Time
		if err := rows.Scan(&id, &schedType, &cronExpr, &intervalSec, &runAt); err != nil {
			continue
		}

		sched := Schedule{ID: id, ScheduleType: schedType, CronExpr: cronExpr, IntervalSeconds: intervalSec, RunAt: runAt}
		next := s.computeNextRun(sched, now)
		if next == nil && schedType == "once" && runAt != nil {
			next = runAt
		}
		if next != nil {
			_, _ = s.db.ExecContext(ctx,
				`UPDATE agent_schedules SET next_run_at=$1 WHERE id=$2`, next, id)
		}
	}
}

// ── NATS ──────────────────────────────────────────────────────────────────────

func (s *Scheduler) publishEvent(subject string, data map[string]interface{}) {
	if s.nc == nil {
		return
	}
	payload, _ := json.Marshal(data)
	_ = s.nc.Publish(subject, payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (s *Scheduler) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "scheduler-service"})
	})

	// List all schedules
	r.GET("/schedules", func(c *gin.Context) {
		rows, err := s.db.QueryContext(c.Request.Context(),
			`SELECT id, agent_id, schedule_type, cron_expr, interval_seconds,
			        run_at, enabled, last_run_at, next_run_at, run_count, max_runs
			 FROM agent_schedules ORDER BY created_at DESC`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var schedules []map[string]interface{}
		for rows.Next() {
			var id, agentID, schedType string
			var cronExpr *string
			var intervalSec, maxRuns *int
			var runAt, lastRun, nextRun *time.Time
			var enabled bool
			var runCount int
			if err := rows.Scan(&id, &agentID, &schedType, &cronExpr, &intervalSec,
				&runAt, &enabled, &lastRun, &nextRun, &runCount, &maxRuns); err != nil {
				continue
			}
			schedules = append(schedules, map[string]interface{}{
				"id": id, "agent_id": agentID, "schedule_type": schedType,
				"cron_expr": cronExpr, "interval_seconds": intervalSec,
				"run_at": runAt, "enabled": enabled,
				"last_run_at": lastRun, "next_run_at": nextRun,
				"run_count": runCount, "max_runs": maxRuns,
			})
		}
		if schedules == nil {
			schedules = []map[string]interface{}{}
		}
		c.JSON(200, schedules)
	})

	// Create schedule
	r.POST("/schedules", func(c *gin.Context) {
		var req struct {
			AgentID         string  `json:"agent_id" binding:"required"`
			ScheduleType    string  `json:"schedule_type" binding:"required"`
			CronExpr        *string `json:"cron_expr"`
			IntervalSeconds *int    `json:"interval_seconds"`
			RunAt           *string `json:"run_at"`
			Timezone        string  `json:"timezone"`
			MaxRuns         *int    `json:"max_runs"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		if req.Timezone == "" {
			req.Timezone = "UTC"
		}

		// Validate cron
		if req.ScheduleType == "cron" && req.CronExpr != nil {
			if _, err := s.cronP.Parse(*req.CronExpr); err != nil {
				c.JSON(400, gin.H{"error": "invalid cron expression: " + err.Error()})
				return
			}
		}

		var runAt *time.Time
		if req.RunAt != nil {
			t, err := time.Parse(time.RFC3339, *req.RunAt)
			if err != nil {
				c.JSON(400, gin.H{"error": "invalid run_at format, use RFC3339"})
				return
			}
			runAt = &t
		}

		var id string
		err := s.db.QueryRowContext(c.Request.Context(),
			`INSERT INTO agent_schedules
			 (agent_id, schedule_type, cron_expr, interval_seconds, run_at, timezone, max_runs)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
			 RETURNING id::text`,
			req.AgentID, req.ScheduleType, req.CronExpr, req.IntervalSeconds,
			runAt, req.Timezone, req.MaxRuns,
		).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// Compute and set next_run_at
		sched := Schedule{
			ID: id, ScheduleType: req.ScheduleType,
			CronExpr: req.CronExpr, IntervalSeconds: req.IntervalSeconds, RunAt: runAt,
		}
		next := s.computeNextRun(sched, time.Now().UTC())
		if next == nil && req.ScheduleType == "once" && runAt != nil {
			next = runAt
		}
		if next != nil {
			_, _ = s.db.ExecContext(c.Request.Context(),
				`UPDATE agent_schedules SET next_run_at=$1 WHERE id=$2`, next, id)
		}

		c.JSON(201, gin.H{"id": id, "next_run_at": next})
	})

	// Delete schedule
	r.DELETE("/schedules/:id", func(c *gin.Context) {
		id := c.Param("id")
		_, err := s.db.ExecContext(c.Request.Context(),
			`DELETE FROM agent_schedules WHERE id=$1::uuid`, id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	})

	// Toggle enable/disable
	r.PATCH("/schedules/:id", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.Enabled != nil {
			_, _ = s.db.ExecContext(c.Request.Context(),
				`UPDATE agent_schedules SET enabled=$1 WHERE id=$2::uuid`, *req.Enabled, id)
		}
		c.JSON(200, gin.H{"id": id})
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting scheduler service")

	// ── Connect PostgreSQL ─────────────────────────────────────────────
	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil {
			if pingErr := db.PingContext(context.Background()); pingErr == nil {
				break
			}
		}
		logger.Info("waiting for postgres", "attempt", i+1)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		logger.Error("postgres connection failed", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)
	defer db.Close()

	// ── Connect NATS ───────────────────────────────────────────────────
	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL,
			natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(2*time.Second),
			natsgo.Name("scheduler-service"),
		)
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Error("nats connection failed", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

	scheduler := NewScheduler(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go scheduler.Run(ctx)

	router := scheduler.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("scheduler http listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
