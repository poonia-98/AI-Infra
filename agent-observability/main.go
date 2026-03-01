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
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	DatabaseURL      string
	NatsURL          string
	Port             string
	SnapshotInterval time.Duration
}

func loadConfig() Config {
	interval, _ := time.ParseDuration(getEnv("SNAPSHOT_INTERVAL", "1h"))
	return Config{
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:          getEnv("NATS_URL", "nats://nats:4222"),
		Port:             getEnv("PORT", "8098"),
		SnapshotInterval: interval,
	}
}

var (
	tracesRecorded = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_observability_traces_recorded_total"})
	diagsCreated   = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_observability_diagnostics_created_total"})
)

func init() {
	prometheus.MustRegister(tracesRecorded, diagsCreated)
}

type Observability struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *Observability {
	return &Observability{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── NATS event subscribers ─────────────────────────────────────────────────

func (o *Observability) Subscribe(ctx context.Context) {
	// Ingest execution traces from agents via NATS
	o.nc.Subscribe("observability.trace", func(msg *natsgo.Msg) {
		var trace struct {
			ExecutionID    string                 `json:"execution_id"`
			AgentID        string                 `json:"agent_id"`
			OrganisationID string                 `json:"organisation_id"`
			TraceID        string                 `json:"trace_id"`
			RootSpanID     string                 `json:"root_span_id"`
			StartTime      string                 `json:"start_time"`
			EndTime        string                 `json:"end_time"`
			Status         string                 `json:"status"`
			ParentTraceID  string                 `json:"parent_trace_id"`
			WorkflowExecID string                 `json:"workflow_exec_id"`
			Spans          []interface{}          `json:"spans"`
			Attributes     map[string]interface{} `json:"attributes"`
			Events         []interface{}          `json:"events"`
			ResourceUsage  map[string]interface{} `json:"resource_usage"`
		}
		if err := json.Unmarshal(msg.Data, &trace); err != nil {
			return
		}

		spansJSON, _ := json.Marshal(trace.Spans)
		attrsJSON, _ := json.Marshal(trace.Attributes)
		eventsJSON, _ := json.Marshal(trace.Events)
		resJSON, _ := json.Marshal(trace.ResourceUsage)

		var startTime time.Time
		if trace.StartTime != "" {
			startTime, _ = time.Parse(time.RFC3339Nano, trace.StartTime)
		} else {
			startTime = time.Now()
		}
		var endTime *time.Time
		if trace.EndTime != "" {
			t, _ := time.Parse(time.RFC3339Nano, trace.EndTime)
			endTime = &t
		}

		_, err := o.db.ExecContext(ctx, `
			INSERT INTO execution_traces
			  (execution_id, agent_id, organisation_id, trace_id, root_span_id,
			   start_time, end_time, status, parent_trace_id, workflow_exec_id,
			   spans, attributes, events, resource_usage)
			VALUES
			  (NULLIF($1,'')::uuid, $2::uuid, NULLIF($3,'')::uuid, $4, $5,
			   $6, $7, $8, NULLIF($9,''), NULLIF($10,'')::uuid,
			   $11::jsonb, $12::jsonb, $13::jsonb, $14::jsonb)
			ON CONFLICT DO NOTHING`,
			trace.ExecutionID, trace.AgentID, trace.OrganisationID,
			trace.TraceID, trace.RootSpanID, startTime, endTime,
			orDefault(trace.Status, "ok"), trace.ParentTraceID, trace.WorkflowExecID,
			string(spansJSON), string(attrsJSON), string(eventsJSON), string(resJSON))

		if err == nil {
			tracesRecorded.Inc()
		}
	})

	// Agent execution failed → create diagnostic
	o.nc.Subscribe("agents.events", func(msg *natsgo.Msg) {
		var event struct {
			EventType      string                 `json:"event_type"`
			AgentID        string                 `json:"agent_id"`
			ExecutionID    string                 `json:"execution_id"`
			OrganisationID string                 `json:"organisation_id"`
			Error          string                 `json:"error"`
			ExitCode       int                    `json:"exit_code"`
			LastLogLines   []string               `json:"last_log_lines"`
			ResourceUsage  map[string]interface{} `json:"resource_usage"`
		}
		json.Unmarshal(msg.Data, &event)

		if event.EventType != "agent.failed" && event.EventType != "execution.failed" {
			return
		}

		category := o.classifyFailure(event.Error, event.ExitCode)
		probable, suggestion := o.diagnose(category, event.Error)
		logsJSON, _ := json.Marshal(event.LastLogLines)
		resJSON, _ := json.Marshal(event.ResourceUsage)

		o.db.ExecContext(ctx, `
			INSERT INTO failure_diagnostics
			  (execution_id, agent_id, organisation_id, failure_category,
			   error_message, probable_cause, suggested_fix,
			   container_exit_code, last_log_lines, resource_usage)
			VALUES
			  (NULLIF($1,'')::uuid, $2::uuid, NULLIF($3,'')::uuid,
			   $4, $5, $6, $7, $8, $9::text[], $10::jsonb)`,
			event.ExecutionID, event.AgentID, event.OrganisationID,
			category, event.Error, probable, suggestion,
			event.ExitCode, string(logsJSON), string(resJSON))

		diagsCreated.Inc()
	})

	// Update agent lineage when agents are spawned
	o.nc.Subscribe("agent.spawned", func(msg *natsgo.Msg) {
		var event struct {
			ParentAgentID  string `json:"parent_agent_id"`
			ChildAgentID   string `json:"child_agent_id"`
			OrganisationID string `json:"organisation_id"`
		}
		json.Unmarshal(msg.Data, &event)
		o.updateLineage(ctx, event.ParentAgentID, event.ChildAgentID, event.OrganisationID)
	})
}

func (o *Observability) classifyFailure(errMsg string, exitCode int) string {
	switch {
	case exitCode == 137:
		return "oom"
	case exitCode == 124:
		return "timeout"
	case contains(errMsg, "permission denied", "access denied"):
		return "sandbox_violation"
	case contains(errMsg, "connection refused", "dial tcp", "network"):
		return "network"
	case contains(errMsg, "config", "configuration", "env"):
		return "config"
	case exitCode != 0 && exitCode != 1:
		return "runtime"
	default:
		return "crash"
	}
}

func (o *Observability) diagnose(category, errMsg string) (probable, suggestion string) {
	switch category {
	case "oom":
		return "Agent exceeded memory limit (OOM killed by kernel)",
			"Increase memory_limit in agent config or optimize memory usage"
	case "timeout":
		return "Agent execution exceeded time limit",
			"Increase timeout_seconds or break task into smaller steps"
	case "sandbox_violation":
		return "Agent attempted a blocked operation (syscall, network, filesystem)",
			"Review sandbox_policy and grant necessary permissions"
	case "network":
		return "Network connectivity issue — target host unreachable",
			"Check network_mode in sandbox policy and verify target service is running"
	case "config":
		return "Agent failed due to missing or invalid configuration",
			"Verify all required environment variables are set and valid"
	case "runtime":
		return fmt.Sprintf("Container runtime error (exit code %s)", errMsg),
			"Check agent logs for stack trace. May need to update runtime image"
	default:
		return "Agent process crashed unexpectedly",
			"Review agent logs for error details"
	}
}

func (o *Observability) updateLineage(ctx context.Context, parentID, childID, orgID string) {
	// Get parent's ancestors
	var parentAncestors []byte
	o.db.QueryRowContext(ctx, `
		SELECT ancestors FROM agent_lineage WHERE agent_id=$1::uuid`, parentID).Scan(&parentAncestors)

	var ancestors []string
	if len(parentAncestors) > 0 {
		json.Unmarshal(parentAncestors, &ancestors)
	}
	ancestors = append(ancestors, parentID)
	depth := len(ancestors)

	rootID := parentID
	if len(ancestors) > 0 {
		rootID = ancestors[0]
	}

	ancestorsJSON, _ := json.Marshal(ancestors)
	o.db.ExecContext(ctx, `
		INSERT INTO agent_lineage (agent_id, organisation_id, ancestors, depth, root_agent_id)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid[], $4, $5::uuid)
		ON CONFLICT (agent_id) DO UPDATE SET ancestors=EXCLUDED.ancestors, depth=EXCLUDED.depth`,
		childID, orgID, fmt.Sprintf("{%s}", joinUUIDs(ancestors)), depth, rootID)
}

// ── Performance snapshot aggregation ──────────────────────────────────────

func (o *Observability) AggregateSnapshots(ctx context.Context) {
	// Aggregate last hour's execution data per agent
	_, err := o.db.ExecContext(ctx, `
		INSERT INTO performance_snapshots
		  (agent_id, organisation_id, period_start, period_end, period_type,
		   total_executions, successful_execs, failed_execs,
		   avg_duration_ms, p95_duration_ms, error_rate)
		SELECT
		  e.agent_id,
		  a.organisation_id,
		  date_trunc('hour', NOW() - INTERVAL '1 hour') as period_start,
		  date_trunc('hour', NOW()) as period_end,
		  'hour' as period_type,
		  COUNT(*) as total,
		  COUNT(*) FILTER (WHERE e.status='completed') as success,
		  COUNT(*) FILTER (WHERE e.status='failed') as failed,
		  AVG(e.duration_ms)::BIGINT,
		  PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY e.duration_ms)::BIGINT,
		  COUNT(*) FILTER (WHERE e.status='failed')::float / NULLIF(COUNT(*),0)
		FROM executions e
		JOIN agents a ON a.id=e.agent_id
		WHERE e.created_at >= date_trunc('hour', NOW() - INTERVAL '1 hour')
		  AND e.created_at < date_trunc('hour', NOW())
		GROUP BY e.agent_id, a.organisation_id
		ON CONFLICT (agent_id, period_start, period_type) DO UPDATE SET
		  total_executions=EXCLUDED.total_executions,
		  successful_execs=EXCLUDED.successful_execs,
		  failed_execs=EXCLUDED.failed_execs,
		  avg_duration_ms=EXCLUDED.avg_duration_ms,
		  p95_duration_ms=EXCLUDED.p95_duration_ms,
		  error_rate=EXCLUDED.error_rate`)
	if err != nil {
		o.logger.Error("snapshot aggregation failed", "error", err)
	}
}

func (o *Observability) RunSnapshotWorker(ctx context.Context) {
	ticker := time.NewTicker(o.cfg.SnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.AggregateSnapshots(ctx)
		}
	}
}

// ── HTTP API ───────────────────────────────────────────────────────────────

func (o *Observability) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "agent-observability"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := o.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Get traces for an execution
	r.GET("/executions/:exec_id/traces", func(c *gin.Context) {
		rows, err := o.db.QueryContext(c.Request.Context(), `
			SELECT id::text, trace_id, root_span_id, start_time, end_time,
			       duration_ms, status, spans, attributes, resource_usage
			FROM execution_traces WHERE execution_id=$1::uuid
			ORDER BY start_time`, c.Param("exec_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var traces []map[string]interface{}
		for rows.Next() {
			var id, traceID, spanID, status string
			var durationMs *int64
			var startTime time.Time
			var endTime *time.Time
			var spans, attrs, resources []byte
			rows.Scan(&id, &traceID, &spanID, &startTime, &endTime, &durationMs, &status, &spans, &attrs, &resources)
			var spansData, attrsData, resData interface{}
			json.Unmarshal(spans, &spansData)
			json.Unmarshal(attrs, &attrsData)
			json.Unmarshal(resources, &resData)
			traces = append(traces, map[string]interface{}{
				"id": id, "trace_id": traceID, "root_span_id": spanID,
				"start_time": startTime, "end_time": endTime, "duration_ms": durationMs,
				"status": status, "spans": spansData, "attributes": attrsData,
				"resource_usage": resData,
			})
		}
		if traces == nil {
			traces = []map[string]interface{}{}
		}
		c.JSON(200, traces)
	})

	// Get agent lineage
	r.GET("/agents/:agent_id/lineage", func(c *gin.Context) {
		row := o.db.QueryRowContext(c.Request.Context(), `
			SELECT agent_id::text, ancestors::text[], depth, root_agent_id::text
			FROM agent_lineage WHERE agent_id=$1::uuid`, c.Param("agent_id"))
		var agentID, rootID string
		var ancestors []string
		var depth int
		if err := row.Scan(&agentID, &ancestors, &depth, &rootID); err != nil {
			c.JSON(200, gin.H{"agent_id": c.Param("agent_id"), "ancestors": []string{}, "depth": 0})
			return
		}
		c.JSON(200, gin.H{
			"agent_id": agentID, "ancestors": ancestors,
			"depth": depth, "root_agent_id": rootID,
		})
	})

	// Get performance snapshots for agent
	r.GET("/agents/:agent_id/performance", func(c *gin.Context) {
		period := c.DefaultQuery("period", "hour")
		rows, err := o.db.QueryContext(c.Request.Context(), `
			SELECT period_start, period_end, total_executions, successful_execs,
			       failed_execs, avg_duration_ms, p95_duration_ms, p99_duration_ms,
			       avg_cpu_pct, avg_memory_mb, error_rate
			FROM performance_snapshots
			WHERE agent_id=$1::uuid AND period_type=$2
			ORDER BY period_start DESC LIMIT 168`, c.Param("agent_id"), period)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var snapshots []map[string]interface{}
		for rows.Next() {
			var ps, pe time.Time
			var total, success, failed int
			var avgMs, p95Ms, p99Ms int64
			var avgCPU, avgMem, errRate float64
			rows.Scan(&ps, &pe, &total, &success, &failed, &avgMs, &p95Ms, &p99Ms, &avgCPU, &avgMem, &errRate)
			snapshots = append(snapshots, map[string]interface{}{
				"period_start": ps, "period_end": pe,
				"total_executions": total, "successful_execs": success, "failed_execs": failed,
				"avg_duration_ms": avgMs, "p95_duration_ms": p95Ms, "p99_duration_ms": p99Ms,
				"avg_cpu_pct": avgCPU, "avg_memory_mb": avgMem, "error_rate": errRate,
			})
		}
		if snapshots == nil {
			snapshots = []map[string]interface{}{}
		}
		c.JSON(200, snapshots)
	})

	// Get failure diagnostics
	r.GET("/agents/:agent_id/diagnostics", func(c *gin.Context) {
		rows, err := o.db.QueryContext(c.Request.Context(), `
			SELECT id::text, execution_id::text, failure_category, error_code,
			       error_message, probable_cause, suggested_fix, resolved, created_at
			FROM failure_diagnostics WHERE agent_id=$1::uuid
			ORDER BY created_at DESC LIMIT 100`, c.Param("agent_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var diags []map[string]interface{}
		for rows.Next() {
			var id, execID, cat, errCode, errMsg, cause, fix string
			var resolved bool
			var ts time.Time
			rows.Scan(&id, &execID, &cat, &errCode, &errMsg, &cause, &fix, &resolved, &ts)
			diags = append(diags, map[string]interface{}{
				"id": id, "execution_id": execID, "failure_category": cat,
				"error_code": errCode, "error_message": errMsg,
				"probable_cause": cause, "suggested_fix": fix,
				"resolved": resolved, "created_at": ts,
			})
		}
		if diags == nil {
			diags = []map[string]interface{}{}
		}
		c.JSON(200, diags)
	})

	// Ingest trace via REST (alternative to NATS)
	r.POST("/traces", func(c *gin.Context) {
		payload, _ := c.GetRawData()
		o.nc.Publish("observability.trace", payload)
		c.JSON(202, gin.H{"status": "accepted"})
	})

	// Org-level analytics
	r.GET("/orgs/:org_id/analytics", func(c *gin.Context) {
		var totalExec, failedExec int
		var avgDuration float64
		o.db.QueryRowContext(c.Request.Context(), `
			SELECT COUNT(*), COUNT(*) FILTER (WHERE status='failed'),
			       COALESCE(AVG(duration_ms), 0)
			FROM executions e
			JOIN agents a ON a.id=e.agent_id
			WHERE a.organisation_id=$1::uuid
			  AND e.created_at >= NOW() - INTERVAL '30 days'`,
			c.Param("org_id")).Scan(&totalExec, &failedExec, &avgDuration)

		var agentCount int
		o.db.QueryRowContext(c.Request.Context(), `
			SELECT COUNT(*) FROM agents WHERE organisation_id=$1::uuid`, c.Param("org_id")).Scan(&agentCount)

		c.JSON(200, gin.H{
			"organisation_id":    c.Param("org_id"),
			"period":             "30d",
			"total_executions":   totalExec,
			"failed_executions":  failedExec,
			"error_rate":         float64(failedExec) / max1(float64(totalExec)),
			"avg_duration_ms":    avgDuration,
			"total_agents":       agentCount,
		})
	})

	return r
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting agent-observability", "port", cfg.Port)

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
	db.SetMaxOpenConns(20)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.Name("agent-observability"))
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

	obs := New(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	obs.Subscribe(ctx)
	go obs.RunSnapshotWorker(ctx)

	router := obs.setupRouter()
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
func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
func joinUUIDs(ids []string) string {
	result := ""
	for i, id := range ids {
		if i > 0 {
			result += ","
		}
		result += id
	}
	return result
}
func contains(s, substr1, substr2 string) bool {
	for _, sub := range []string{substr1, substr2} {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
func max1(x float64) float64 {
	if x < 1 {
		return 1
	}
	return x
}