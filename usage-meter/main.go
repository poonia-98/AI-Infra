package main

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
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	DatabaseURL  string
	NatsURL      string
	Port         string
	FlushEvery   time.Duration
	RetentionDays int
}

func loadConfig() Config {
	flush, _ := time.ParseDuration(getEnv("FLUSH_INTERVAL", "30s"))
	return Config{
		DatabaseURL:   getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:       getEnv("NATS_URL", "nats://nats:4222"),
		Port:          getEnv("PORT", "8100"),
		FlushEvery:    flush,
		RetentionDays: 90,
	}
}

var (
	usageEventsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_usage_events_received_total",
	}, []string{"resource_type"})
	usageEventsFlushed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_usage_events_flushed_total",
	})
)

func init() {
	prometheus.MustRegister(usageEventsReceived, usageEventsFlushed)
}

type UsageEvent struct {
	OrgID        string                 `json:"organisation_id"`
	SubscriptionID string              `json:"subscription_id,omitempty"`
	ResourceType string                 `json:"resource_type"`
	Quantity     int64                  `json:"quantity"`
	Unit         string                 `json:"unit"`
	ResourceID   string                 `json:"resource_id,omitempty"`
	ResourceMeta map[string]interface{} `json:"resource_meta,omitempty"`
	PeriodStart  time.Time              `json:"period_start"`
	PeriodEnd    time.Time              `json:"period_end"`
}

type UsageMeter struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
	mu     sync.Mutex
	buffer []UsageEvent
}

func NewUsageMeter(cfg Config, db *sql.DB, nc *natsgo.Conn) *UsageMeter {
	return &UsageMeter{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		buffer: make([]UsageEvent, 0, 1000),
	}
}

func (m *UsageMeter) Record(event UsageEvent) {
	if event.Unit == "" {
		event.Unit = "count"
	}
	if event.PeriodStart.IsZero() {
		now := time.Now().UTC()
		event.PeriodStart = now.Truncate(time.Hour)
		event.PeriodEnd = event.PeriodStart.Add(time.Hour)
	}

	m.mu.Lock()
	m.buffer = append(m.buffer, event)
	m.mu.Unlock()

	usageEventsReceived.With(prometheus.Labels{"resource_type": event.ResourceType}).Inc()
}

func (m *UsageMeter) Flush(ctx context.Context) {
	m.mu.Lock()
	if len(m.buffer) == 0 {
		m.mu.Unlock()
		return
	}
	batch := m.buffer
	m.buffer = make([]UsageEvent, 0, 1000)
	m.mu.Unlock()

	// Batch insert usage records
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		m.logger.Error("flush tx failed", "error", err)
		return
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_records
		  (organisation_id, subscription_id, resource_type, quantity, unit,
		   resource_id, resource_meta, period_start, period_end)
		VALUES
		  ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5,
		   NULLIF($6,'')::uuid, $7::jsonb, $8, $9)`)
	if err != nil {
		tx.Rollback()
		return
	}
	defer stmt.Close()

	for _, ev := range batch {
		metaJSON, _ := json.Marshal(ev.ResourceMeta)
		_, err := stmt.ExecContext(ctx,
			ev.OrgID, ev.SubscriptionID, ev.ResourceType, ev.Quantity, ev.Unit,
			ev.ResourceID, string(metaJSON), ev.PeriodStart, ev.PeriodEnd)
		if err != nil {
			m.logger.Error("insert usage record failed", "error", err, "resource_type", ev.ResourceType)
		}
	}

	if err := tx.Commit(); err != nil {
		m.logger.Error("flush commit failed", "error", err)
		return
	}

	usageEventsFlushed.Add(float64(len(batch)))
	m.logger.Debug("usage flushed", "count", len(batch))
}

func (m *UsageMeter) Subscribe(ctx context.Context) {
	// Agent execution events
	m.nc.Subscribe("agents.events", func(msg *natsgo.Msg) {
		var event struct {
			EventType      string `json:"event_type"`
			AgentID        string `json:"agent_id"`
			OrganisationID string `json:"organisation_id"`
			ExecutionID    string `json:"execution_id"`
		}
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			return
		}
		if event.OrganisationID == "" {
			return
		}

		switch event.EventType {
		case "agent.started":
			m.Record(UsageEvent{
				OrgID: event.OrganisationID, ResourceType: "agent_start",
				Quantity: 1, Unit: "count", ResourceID: event.AgentID,
				ResourceMeta: map[string]interface{}{"agent_id": event.AgentID},
			})
		case "execution.completed":
			m.Record(UsageEvent{
				OrgID: event.OrganisationID, ResourceType: "execution",
				Quantity: 1, Unit: "count", ResourceID: event.ExecutionID,
				ResourceMeta: map[string]interface{}{"agent_id": event.AgentID},
			})
		}
	})

	// Workflow execution events
	m.nc.Subscribe("workflows.events", func(msg *natsgo.Msg) {
		var event struct {
			EventType      string `json:"event_type"`
			WorkflowID     string `json:"workflow_id"`
			ExecutionID    string `json:"execution_id"`
			OrganisationID string `json:"organisation_id"`
			DurationMs     int64  `json:"duration_ms"`
		}
		json.Unmarshal(msg.Data, &event)
		if event.OrganisationID == "" {
			return
		}
		if event.EventType == "workflow.completed" || event.EventType == "workflow.failed" {
			m.Record(UsageEvent{
				OrgID: event.OrganisationID, ResourceType: "workflow_execution",
				Quantity: 1, Unit: "count", ResourceID: event.ExecutionID,
				ResourceMeta: map[string]interface{}{
					"workflow_id": event.WorkflowID, "duration_ms": event.DurationMs,
				},
			})
		}
	})

	// API call tracking (from backend rate limiter)
	m.nc.Subscribe("api.usage", func(msg *natsgo.Msg) {
		var event struct {
			OrgID    string `json:"organisation_id"`
			Endpoint string `json:"endpoint"`
			Method   string `json:"method"`
		}
		json.Unmarshal(msg.Data, &event)
		if event.OrgID == "" {
			return
		}
		m.Record(UsageEvent{
			OrgID: event.OrgID, ResourceType: "api_call",
			Quantity: 1, Unit: "count",
			ResourceMeta: map[string]interface{}{"endpoint": event.Endpoint, "method": event.Method},
		})
	})
}

func (m *UsageMeter) ComputePeriodCost(ctx context.Context, orgID string, periodStart, periodEnd time.Time) (map[string]interface{}, error) {
	// Get subscription plan pricing
	row := m.db.QueryRowContext(ctx, `
		SELECT bp.price_per_exec, bp.price_per_agent_hour,
		       bp.price_per_workflow_exec, bp.price_per_api_call,
		       bp.free_executions, bp.free_agent_hours, bp.free_api_calls
		FROM subscriptions s JOIN billing_plans bp ON bp.id=s.plan_id
		WHERE s.organisation_id=$1::uuid AND s.status='active'`, orgID)

	var pricePerExec, pricePerAgentHour, pricePerWorkflow, pricePerAPI int64
	var freeExec, freeAgentHours, freeAPI int64
	if err := row.Scan(&pricePerExec, &pricePerAgentHour, &pricePerWorkflow, &pricePerAPI,
		&freeExec, &freeAgentHours, &freeAPI); err != nil {
		// Default to free plan pricing
		pricePerExec, pricePerAgentHour, pricePerWorkflow, pricePerAPI = 0, 0, 0, 0
		freeExec, freeAgentHours, freeAPI = 1000, 100, 10000
	}

	// Sum usage by type
	rows, err := m.db.QueryContext(ctx, `
		SELECT resource_type, SUM(quantity) as total
		FROM usage_records
		WHERE organisation_id=$1::uuid
		  AND period_start >= $2 AND period_end <= $3
		GROUP BY resource_type`, orgID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	usage := map[string]int64{}
	for rows.Next() {
		var rtype string
		var qty int64
		rows.Scan(&rtype, &qty)
		usage[rtype] = qty
	}

	// Calculate costs
	execCost := int64(0)
	if execs := usage["execution"]; execs > freeExec {
		execCost = (execs - freeExec) * pricePerExec
	}
	workflowCost := usage["workflow_execution"] * pricePerWorkflow
	apiCost := int64(0)
	if calls := usage["api_call"]; calls > freeAPI {
		apiCost = (calls - freeAPI) * pricePerAPI
	}

	totalCostMC := execCost + workflowCost + apiCost // in micro-cents

	return map[string]interface{}{
		"organisation_id": orgID,
		"period_start":    periodStart,
		"period_end":      periodEnd,
		"usage":           usage,
		"costs": map[string]interface{}{
			"executions_cost_mc":         execCost,
			"workflow_executions_cost_mc": workflowCost,
			"api_calls_cost_mc":           apiCost,
			"total_cost_mc":               totalCostMC,
			"total_cost_cents":            totalCostMC / 1_000_000,
		},
	}, nil
}

func (m *UsageMeter) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "usage-meter"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := m.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Record a usage event manually
	r.POST("/usage", func(c *gin.Context) {
		var ev UsageEvent
		if err := c.ShouldBindJSON(&ev); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		m.Record(ev)
		c.JSON(202, gin.H{"status": "recorded"})
	})

	// Query usage for an org
	r.GET("/orgs/:org_id/usage", func(c *gin.Context) {
		orgID := c.Param("org_id")
		sinceStr := c.DefaultQuery("since", time.Now().UTC().Format("2006-01-02"))
		untilStr := c.DefaultQuery("until", time.Now().UTC().Format("2006-01-02T15:04:05Z"))

		since, _ := time.Parse("2006-01-02", sinceStr)
		until, _ := time.Parse(time.RFC3339, untilStr)
		if until.IsZero() {
			until = time.Now().UTC()
		}

		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT resource_type, SUM(quantity) as total, unit
			FROM usage_records
			WHERE organisation_id=$1::uuid
			  AND recorded_at >= $2 AND recorded_at <= $3
			GROUP BY resource_type, unit
			ORDER BY resource_type`,
			orgID, since, until)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		usage := []map[string]interface{}{}
		for rows.Next() {
			var rtype, unit string
			var total int64
			rows.Scan(&rtype, &total, &unit)
			usage = append(usage, map[string]interface{}{
				"resource_type": rtype, "quantity": total, "unit": unit,
			})
		}
		c.JSON(200, gin.H{"organisation_id": orgID, "since": since, "until": until, "usage": usage})
	})

	// Get cost estimate for a period
	r.GET("/orgs/:org_id/cost-estimate", func(c *gin.Context) {
		orgID := c.Param("org_id")
		// Default to current month
		now := time.Now().UTC()
		periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		periodEnd := now

		cost, err := m.ComputePeriodCost(c.Request.Context(), orgID, periodStart, periodEnd)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, cost)
	})

	// Force flush (for testing)
	r.POST("/flush", func(c *gin.Context) {
		m.Flush(c.Request.Context())
		c.JSON(200, gin.H{"status": "flushed"})
	})

	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting usage-meter", "port", cfg.Port, "flush_every", cfg.FlushEvery)

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
	db.SetMaxOpenConns(10)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.Name("usage-meter"))
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

	meter := NewUsageMeter(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	meter.Subscribe(ctx)

	go func() {
		ticker := time.NewTicker(cfg.FlushEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				meter.Flush(context.Background())
				return
			case <-ticker.C:
				meter.Flush(ctx)
			}
		}
	}()

	router := meter.setupRouter()
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
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
	logger.Info("usage-meter stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func formatCost(microCents int64) string {
	cents := microCents / 1_000_000
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}