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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ── Config ────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL  string
	NatsURL      string
	BackendURL   string
	ControlPlaneAPIKey string
	MetricsURL   string
	Port         string
	EvalInterval time.Duration
}

func loadConfig() Config {
	eval, _ := time.ParseDuration(getEnv("EVAL_INTERVAL", "15s"))
	return Config{
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:      getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:   getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		ControlPlaneAPIKey: getEnv("CONTROL_PLANE_API_KEY", ""),
		MetricsURL:   getEnv("METRICS_URL", "http://metrics-collector:8083"),
		Port:         getEnv("PORT", "8093"),
		EvalInterval: eval,
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────

var (
	scaleUpEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_autoscaler_scale_up_total",
	}, []string{"agent_id", "metric"})
	scaleDownEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_autoscaler_scale_down_total",
	}, []string{"agent_id", "metric"})
	currentReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_autoscaler_replicas",
	}, []string{"agent_id"})
)

func init() {
	prometheus.MustRegister(scaleUpEvents, scaleDownEvents, currentReplicas)
}

// ── Domain ────────────────────────────────────────────────────────────────

type ScalePolicy struct {
	ID                   string
	AgentID              string
	OrganisationID       string
	Enabled              bool
	MinReplicas          int
	MaxReplicas          int
	ScaleMetric          string
	ScaleUpThreshold     float64
	ScaleDownThreshold   float64
	ScaleUpCooldownSec   int
	ScaleDownCooldownSec int
	ScaleIncrement       int
	CurrentReplicas      int
	LastScaleAt          *time.Time
}

// ── Autoscaler ────────────────────────────────────────────────────────────

type Autoscaler struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func NewAutoscaler(cfg Config, db *sql.DB, nc *natsgo.Conn) *Autoscaler {
	return &Autoscaler{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 10 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

func (a *Autoscaler) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.EvalInterval)
	defer ticker.Stop()
	a.logger.Info("autoscaler started", "interval", a.cfg.EvalInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.evaluate(ctx)
		}
	}
}

func (a *Autoscaler) evaluate(ctx context.Context) {
	policies, err := a.loadPolicies(ctx)
	if err != nil {
		a.logger.Error("load policies failed", "error", err)
		return
	}

	for _, policy := range policies {
		metricVal, err := a.fetchMetric(ctx, policy.AgentID, policy.ScaleMetric)
		if err != nil {
			a.logger.Warn("metric fetch failed", "agent_id", policy.AgentID, "metric", policy.ScaleMetric, "error", err)
			continue
		}

		currentReplicas.With(prometheus.Labels{"agent_id": policy.AgentID}).Set(float64(policy.CurrentReplicas))
		now := time.Now()

		if metricVal >= policy.ScaleUpThreshold && policy.CurrentReplicas < policy.MaxReplicas {
			// Check cooldown
			if policy.LastScaleAt != nil && now.Sub(*policy.LastScaleAt) < time.Duration(policy.ScaleUpCooldownSec)*time.Second {
				continue
			}
			desired := min(policy.CurrentReplicas+policy.ScaleIncrement, policy.MaxReplicas)
			a.scaleAgent(ctx, policy, desired, "up", metricVal)

		} else if metricVal <= policy.ScaleDownThreshold && policy.CurrentReplicas > policy.MinReplicas {
			if policy.LastScaleAt != nil && now.Sub(*policy.LastScaleAt) < time.Duration(policy.ScaleDownCooldownSec)*time.Second {
				continue
			}
			desired := max(policy.CurrentReplicas-policy.ScaleIncrement, policy.MinReplicas)
			a.scaleAgent(ctx, policy, desired, "down", metricVal)
		}
	}
}

func (a *Autoscaler) fetchMetric(ctx context.Context, agentID, metric string) (float64, error) {
	switch metric {
	case "queue_depth":
		return a.fetchQueueDepth(ctx, agentID)
	case "cpu_percent", "memory_percent", "latency_ms", "rps":
		return a.fetchPrometheusMetric(ctx, agentID, metric)
	default:
		return a.fetchPrometheusMetric(ctx, agentID, metric)
	}
}

func (a *Autoscaler) fetchQueueDepth(ctx context.Context, agentID string) (float64, error) {
	var count float64
	err := a.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_queues
		WHERE agent_id=$1::uuid AND status='queued'`, agentID).Scan(&count)
	return count, err
}

func (a *Autoscaler) fetchPrometheusMetric(ctx context.Context, agentID, metric string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/metrics/agent/%s/%s/latest", a.cfg.MetricsURL, agentID, metric), nil)
	if err != nil {
		return 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		// Fallback: query from metrics table
		var val float64
		dbErr := a.db.QueryRowContext(ctx, `
			SELECT metric_value FROM metrics
			WHERE agent_id=$1::uuid AND metric_name=$2
			ORDER BY timestamp DESC LIMIT 1`, agentID, metric).Scan(&val)
		return val, dbErr
	}
	defer resp.Body.Close()
	var result struct {
		Value float64 `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Value, nil
}

func (a *Autoscaler) scaleAgent(ctx context.Context, policy ScalePolicy, desired int, direction string, triggerVal float64) {
	// Update replica count in DB
	_, err := a.db.ExecContext(ctx, `
		UPDATE autoscale_policies
		SET current_replicas=$1, last_scale_at=NOW(), updated_at=NOW()
		WHERE id=$2`, desired, policy.ID)
	if err != nil {
		a.logger.Error("update replicas failed", "error", err)
		return
	}

	// Record scale event
	a.db.ExecContext(ctx, `
		INSERT INTO autoscale_events
		  (policy_id, agent_id, direction, from_replicas, to_replicas, trigger_metric, trigger_value)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)`,
		policy.ID, policy.AgentID, direction,
		policy.CurrentReplicas, desired, policy.ScaleMetric, triggerVal)

	// Notify backend to apply scale
	payload, _ := json.Marshal(map[string]interface{}{
		"agent_id":         policy.AgentID,
		"desired_replicas": desired,
		"direction":        direction,
		"trigger_metric":   policy.ScaleMetric,
		"trigger_value":    triggerVal,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		a.cfg.BackendURL+"/api/v1/agents/"+policy.AgentID+"/scale",
		bytes.NewReader(payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		if a.cfg.ControlPlaneAPIKey != "" {
			req.Header.Set("X-Service-API-Key", a.cfg.ControlPlaneAPIKey)
		}
		resp, err := a.http.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	a.publishEvent("agent.scale."+direction, map[string]interface{}{
		"agent_id":       policy.AgentID,
		"from_replicas":  policy.CurrentReplicas,
		"to_replicas":    desired,
		"trigger_metric": policy.ScaleMetric,
		"trigger_value":  triggerVal,
	})

	if direction == "up" {
		scaleUpEvents.With(prometheus.Labels{"agent_id": policy.AgentID, "metric": policy.ScaleMetric}).Inc()
	} else {
		scaleDownEvents.With(prometheus.Labels{"agent_id": policy.AgentID, "metric": policy.ScaleMetric}).Inc()
	}

	a.logger.Info("agent scaled", "agent_id", policy.AgentID, "direction", direction,
		"from", policy.CurrentReplicas, "to", desired, "metric", policy.ScaleMetric, "value", triggerVal)
}

func (a *Autoscaler) loadPolicies(ctx context.Context) ([]ScalePolicy, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id::text, agent_id::text, COALESCE(organisation_id::text,''),
		       enabled, min_replicas, max_replicas, scale_metric,
		       scale_up_threshold, scale_down_threshold,
		       scale_up_cooldown_s, scale_down_cooldown_s,
		       scale_increment, current_replicas, last_scale_at
		FROM autoscale_policies WHERE enabled=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []ScalePolicy
	for rows.Next() {
		var p ScalePolicy
		rows.Scan(&p.ID, &p.AgentID, &p.OrganisationID,
			&p.Enabled, &p.MinReplicas, &p.MaxReplicas, &p.ScaleMetric,
			&p.ScaleUpThreshold, &p.ScaleDownThreshold,
			&p.ScaleUpCooldownSec, &p.ScaleDownCooldownSec,
			&p.ScaleIncrement, &p.CurrentReplicas, &p.LastScaleAt)
		policies = append(policies, p)
	}
	return policies, nil
}

func (a *Autoscaler) publishEvent(subject string, data map[string]interface{}) {
	if a.nc == nil {
		return
	}
	data["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	payload, _ := json.Marshal(data)
	_ = a.nc.Publish("autoscaler.events", payload)
	_ = a.nc.Publish(subject, payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────

func (a *Autoscaler) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "agent-autoscaler"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := a.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Create policy
	r.POST("/policies", func(c *gin.Context) {
		var body struct {
			AgentID              string  `json:"agent_id" binding:"required"`
			MinReplicas          int     `json:"min_replicas"`
			MaxReplicas          int     `json:"max_replicas"`
			ScaleMetric          string  `json:"scale_metric"`
			ScaleUpThreshold     float64 `json:"scale_up_threshold"`
			ScaleDownThreshold   float64 `json:"scale_down_threshold"`
			ScaleUpCooldownSec   int     `json:"scale_up_cooldown_s"`
			ScaleDownCooldownSec int     `json:"scale_down_cooldown_s"`
			ScaleIncrement       int     `json:"scale_increment"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.MinReplicas == 0 {
			body.MinReplicas = 1
		}
		if body.MaxReplicas == 0 {
			body.MaxReplicas = 10
		}
		if body.ScaleMetric == "" {
			body.ScaleMetric = "queue_depth"
		}
		if body.ScaleUpThreshold == 0 {
			body.ScaleUpThreshold = 80
		}
		if body.ScaleDownThreshold == 0 {
			body.ScaleDownThreshold = 20
		}
		if body.ScaleUpCooldownSec == 0 {
			body.ScaleUpCooldownSec = 60
		}
		if body.ScaleDownCooldownSec == 0 {
			body.ScaleDownCooldownSec = 300
		}
		if body.ScaleIncrement == 0 {
			body.ScaleIncrement = 1
		}
		var id string
		err := a.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO autoscale_policies
			  (agent_id, min_replicas, max_replicas, scale_metric,
			   scale_up_threshold, scale_down_threshold,
			   scale_up_cooldown_s, scale_down_cooldown_s, scale_increment)
			VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING id::text`,
			body.AgentID, body.MinReplicas, body.MaxReplicas, body.ScaleMetric,
			body.ScaleUpThreshold, body.ScaleDownThreshold,
			body.ScaleUpCooldownSec, body.ScaleDownCooldownSec, body.ScaleIncrement).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id, "agent_id": body.AgentID})
	})

	// List policies
	r.GET("/policies", func(c *gin.Context) {
		rows, err := a.db.QueryContext(c.Request.Context(), `
			SELECT id::text, agent_id::text, enabled, min_replicas, max_replicas,
			       scale_metric, scale_up_threshold, scale_down_threshold, current_replicas
			FROM autoscale_policies ORDER BY created_at DESC`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var policies []map[string]interface{}
		for rows.Next() {
			var id, agentID, metric string
			var enabled bool
			var min, max, current int
			var up, down float64
			rows.Scan(&id, &agentID, &enabled, &min, &max, &metric, &up, &down, &current)
			policies = append(policies, map[string]interface{}{
				"id": id, "agent_id": agentID, "enabled": enabled,
				"min_replicas": min, "max_replicas": max,
				"scale_metric": metric, "scale_up_threshold": up,
				"scale_down_threshold": down, "current_replicas": current,
			})
		}
		if policies == nil {
			policies = []map[string]interface{}{}
		}
		c.JSON(200, policies)
	})

	// Scale history for an agent
	r.GET("/agents/:agent_id/scale-history", func(c *gin.Context) {
		rows, err := a.db.QueryContext(c.Request.Context(), `
			SELECT ae.direction, ae.from_replicas, ae.to_replicas,
			       ae.trigger_metric, ae.trigger_value, ae.created_at
			FROM autoscale_events ae
			JOIN autoscale_policies ap ON ae.policy_id=ap.id
			WHERE ap.agent_id=$1::uuid
			ORDER BY ae.created_at DESC LIMIT 100`, c.Param("agent_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var events []map[string]interface{}
		for rows.Next() {
			var dir, metric string
			var from, to int
			var val float64
			var ts time.Time
			rows.Scan(&dir, &from, &to, &metric, &val, &ts)
			events = append(events, map[string]interface{}{
				"direction": dir, "from_replicas": from, "to_replicas": to,
				"trigger_metric": metric, "trigger_value": val, "created_at": ts,
			})
		}
		if events == nil {
			events = []map[string]interface{}{}
		}
		c.JSON(200, events)
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting agent-autoscaler", "port", cfg.Port)

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
			natsgo.ReconnectWait(2*time.Second), natsgo.Name("agent-autoscaler"))
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

	scaler := NewAutoscaler(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scaler.Run(ctx)

	router := scaler.setupRouter()
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
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
