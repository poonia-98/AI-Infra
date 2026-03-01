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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL   string
	NatsURL       string
	Port          string
	BatchSize     int
	FlushInterval time.Duration
	WorkerCount   int
}

func loadConfig() Config {
	batchSize, _ := strconv.Atoi(getEnv("BATCH_SIZE", "100"))
	flushInterval, _ := time.ParseDuration(getEnv("FLUSH_INTERVAL", "2s"))
	workers, _ := strconv.Atoi(getEnv("WORKER_COUNT", "4"))
	return Config{
		DatabaseURL:   getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:       getEnv("NATS_URL", "nats://nats:4222"),
		Port:          getEnv("PORT", "8088"),
		BatchSize:     batchSize,
		FlushInterval: flushInterval,
		WorkerCount:   workers,
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────────

var (
	logsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_log_processor_received_total",
		Help: "Total log messages received from NATS",
	}, []string{"level"})

	logsPersisted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_log_processor_persisted_total",
		Help: "Total log messages written to PostgreSQL",
	})

	batchFlushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "platform_log_processor_batch_flush_seconds",
		Help:    "Time to flush a batch to PostgreSQL",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0},
	})
)

func init() {
	prometheus.MustRegister(logsReceived, logsPersisted, batchFlushDuration)
}

// ── Log message (NATS payload) ────────────────────────────────────────────────

type LogMessage struct {
	AgentID     string                 `json:"agent_id"`
	ExecutionID string                 `json:"execution_id,omitempty"`
	ContainerID string                 `json:"container_id,omitempty"`
	Stream      string                 `json:"stream,omitempty"`    // stdout | stderr
	Level       string                 `json:"level,omitempty"`     // info | warn | error
	Message     string                 `json:"message"`
	Source      string                 `json:"source,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	Timestamp   string                 `json:"timestamp,omitempty"`
}

// ── Processor ─────────────────────────────────────────────────────────────────

type LogProcessor struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	queue  chan LogMessage
	logger *slog.Logger
	wg     sync.WaitGroup
}

func NewLogProcessor(cfg Config, db *sql.DB, nc *natsgo.Conn) *LogProcessor {
	return &LogProcessor{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		queue:  make(chan LogMessage, 10000), // 10k buffer
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// Subscribe listens on NATS subjects for log messages.
func (p *LogProcessor) Subscribe(ctx context.Context) error {
	// Primary log stream from executor
	_, err := p.nc.Subscribe("logs.stream", func(msg *natsgo.Msg) {
		var lm LogMessage
		if err := json.Unmarshal(msg.Data, &lm); err != nil {
			return
		}
		if lm.Level == "" {
			lm.Level = "info"
		}
		if lm.Stream == "" {
			lm.Stream = "stdout"
		}
		logsReceived.With(prometheus.Labels{"level": lm.Level}).Inc()
		select {
		case p.queue <- lm:
		default:
			p.logger.Warn("log queue full, dropping message")
		}
	})
	if err != nil {
		return fmt.Errorf("subscribe logs.stream: %w", err)
	}

	// Agent-specific log subjects: agent.logs.<agent_id>
	_, err = p.nc.Subscribe("agent.logs.*", func(msg *natsgo.Msg) {
		// Extract agent_id from subject: agent.logs.{uuid}
		parts := strings.Split(msg.Subject, ".")
		agentID := ""
		if len(parts) == 3 {
			agentID = parts[2]
		}

		var lm LogMessage
		if err := json.Unmarshal(msg.Data, &lm); err != nil {
			return
		}
		if lm.AgentID == "" {
			lm.AgentID = agentID
		}
		if lm.Level == "" {
			lm.Level = "info"
		}
		logsReceived.With(prometheus.Labels{"level": lm.Level}).Inc()
		select {
		case p.queue <- lm:
		default:
		}
	})
	if err != nil {
		return fmt.Errorf("subscribe agent.logs.*: %w", err)
	}

	p.logger.Info("subscribed to NATS log subjects")
	return nil
}

// RunWorkers starts the batch flush workers.
func (p *LogProcessor) RunWorkers(ctx context.Context) {
	for i := 0; i < p.cfg.WorkerCount; i++ {
		p.wg.Add(1)
		go p.worker(ctx, i)
	}
	p.logger.Info("log workers started", "count", p.cfg.WorkerCount)
}

func (p *LogProcessor) worker(ctx context.Context, id int) {
	defer p.wg.Done()
	batch := make([]LogMessage, 0, p.cfg.BatchSize)
	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Drain remaining messages
			for len(p.queue) > 0 {
				msg := <-p.queue
				batch = append(batch, msg)
				if len(batch) >= p.cfg.BatchSize {
					p.flush(batch)
					batch = batch[:0]
				}
			}
			if len(batch) > 0 {
				p.flush(batch)
			}
			return

		case msg := <-p.queue:
			batch = append(batch, msg)
			if len(batch) >= p.cfg.BatchSize {
				p.flush(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				p.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

// flush writes a batch of log messages to PostgreSQL using a single multi-row INSERT.
func (p *LogProcessor) flush(batch []LogMessage) {
	if len(batch) == 0 {
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		p.logger.Error("failed to begin transaction", "error", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// Build bulk INSERT for agent_logs
	valueStrings := make([]string, 0, len(batch))
	valueArgs := make([]interface{}, 0, len(batch)*8)
	i := 1
	for _, msg := range batch {
		meta, _ := json.Marshal(msg.Metadata)
		if string(meta) == "null" {
			meta = []byte("{}")
		}

		var execIDPlaceholder string
		if msg.ExecutionID != "" {
			execIDPlaceholder = fmt.Sprintf("NULLIF($%d,'')::uuid", i+1)
		} else {
			execIDPlaceholder = "NULL"
			// Insert empty string as placeholder — we'll skip that param
		}
		_ = execIDPlaceholder

		ts := time.Now().UTC()
		if msg.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, msg.Timestamp); err == nil {
				ts = parsed
			}
		}

		valueStrings = append(valueStrings, fmt.Sprintf(
			"($%d::uuid, NULLIF($%d,'')::uuid, $%d, $%d, $%d, $%d, $%d, $%d::jsonb, $%d)",
			i, i+1, i+2, i+3, i+4, i+5, i+6, i+7, i+8))

		valueArgs = append(valueArgs,
			nilIfEmpty(msg.AgentID),
			nilIfEmpty(msg.ExecutionID),
			orDefault(msg.Stream, "stdout"),
			orDefault(msg.Level, "info"),
			msg.Message,
			orDefault(msg.Source, "container"),
			nilIfEmpty(msg.ContainerID),
			string(meta),
			ts,
		)
		i += 9
	}

	query := fmt.Sprintf(`
		INSERT INTO agent_logs
		  (agent_id, execution_id, stream, level, message, source, container_id, metadata, logged_at)
		VALUES %s
		ON CONFLICT DO NOTHING`,
		strings.Join(valueStrings, ","))

	_, err = tx.ExecContext(ctx, query, valueArgs...)
	if err != nil {
		p.logger.Error("bulk insert failed", "error", err, "batch_size", len(batch))
		return
	}

	// Update execution_logs counters for each unique execution_id in the batch
	execStats := make(map[string]struct{ total, errors, warns int })
	for _, msg := range batch {
		if msg.ExecutionID == "" {
			continue
		}
		s := execStats[msg.ExecutionID]
		s.total++
		if msg.Level == "error" {
			s.errors++
		} else if msg.Level == "warn" || msg.Level == "warning" {
			s.warns++
		}
		execStats[msg.ExecutionID] = s
	}

	for execID, stats := range execStats {
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO execution_logs (execution_id, agent_id, log_count, error_count, warn_count, first_log_at, last_log_at)
			SELECT $1::uuid, agent_id, $2, $3, $4, NOW(), NOW()
			FROM executions WHERE id=$1::uuid
			ON CONFLICT (execution_id) DO UPDATE
			  SET log_count   = execution_logs.log_count + EXCLUDED.log_count,
			      error_count = execution_logs.error_count + EXCLUDED.error_count,
			      warn_count  = execution_logs.warn_count + EXCLUDED.warn_count,
			      last_log_at = NOW(),
			      updated_at  = NOW()`,
			execID, stats.total, stats.errors, stats.warns)
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("transaction commit failed", "error", err)
		return
	}

	elapsed := time.Since(start)
	batchFlushDuration.Observe(elapsed.Seconds())
	logsPersisted.Add(float64(len(batch)))
	p.logger.Debug("batch flushed", "size", len(batch), "duration_ms", elapsed.Milliseconds())
}

func (p *LogProcessor) Wait() {
	p.wg.Wait()
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (p *LogProcessor) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "log-processor", "queue_depth": len(p.queue)})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := p.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// ── Log query API ──────────────────────────────────────────────────

	// GET /logs/agent/:agent_id?limit=100&offset=0&level=error&since=RFC3339
	r.GET("/logs/agent/:agent_id", func(c *gin.Context) {
		agentID := c.Param("agent_id")
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
		offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
		level := c.Query("level")
		since := c.Query("since")

		if limit > 1000 {
			limit = 1000
		}

		args := []interface{}{agentID, limit, offset}
		filter := ""
		if level != "" {
			filter += fmt.Sprintf(" AND level=$%d", len(args)+1)
			args = append(args, level)
		}
		if since != "" {
			if t, err := time.Parse(time.RFC3339, since); err == nil {
				filter += fmt.Sprintf(" AND logged_at >= $%d", len(args)+1)
				args = append(args, t)
			}
		}

		rows, err := p.db.QueryContext(c.Request.Context(), fmt.Sprintf(`
			SELECT id::text, agent_id::text, COALESCE(execution_id::text,''),
			       stream, level, message, source, COALESCE(container_id,''), logged_at
			FROM agent_logs
			WHERE agent_id=$1::uuid %s
			ORDER BY logged_at DESC
			LIMIT $2 OFFSET $3`, filter), args...)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		var logs []map[string]interface{}
		for rows.Next() {
			var id, agentIDR, execID, stream, lvl, msg, source, containerID string
			var loggedAt time.Time
			if err := rows.Scan(&id, &agentIDR, &execID, &stream, &lvl, &msg,
				&source, &containerID, &loggedAt); err != nil {
				continue
			}
			logs = append(logs, map[string]interface{}{
				"id": id, "agent_id": agentIDR, "execution_id": execID,
				"stream": stream, "level": lvl, "message": msg,
				"source": source, "container_id": containerID, "logged_at": loggedAt,
			})
		}
		if logs == nil {
			logs = []map[string]interface{}{}
		}
		c.JSON(200, logs)
	})

	// GET /logs/execution/:execution_id
	r.GET("/logs/execution/:execution_id", func(c *gin.Context) {
		execID := c.Param("execution_id")
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
		if limit > 2000 {
			limit = 2000
		}

		rows, err := p.db.QueryContext(c.Request.Context(), `
			SELECT id::text, agent_id::text, stream, level, message, source, logged_at
			FROM agent_logs
			WHERE execution_id=$1::uuid
			ORDER BY logged_at ASC
			LIMIT $2`, execID, limit)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		var logs []map[string]interface{}
		for rows.Next() {
			var id, agentID, stream, lvl, msg, source string
			var loggedAt time.Time
			if err := rows.Scan(&id, &agentID, &stream, &lvl, &msg, &source, &loggedAt); err != nil {
				continue
			}
			logs = append(logs, map[string]interface{}{
				"id": id, "agent_id": agentID, "stream": stream,
				"level": lvl, "message": msg, "source": source, "logged_at": loggedAt,
			})
		}
		if logs == nil {
			logs = []map[string]interface{}{}
		}
		c.JSON(200, logs)
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting log-processor", "port", cfg.Port, "batch_size", cfg.BatchSize)

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
		logger.Error("postgres unavailable", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL,
			natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second),
			natsgo.Name("log-processor"))
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

	processor := NewLogProcessor(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := processor.Subscribe(ctx); err != nil {
		logger.Error("failed to subscribe", "error", err)
		os.Exit(1)
	}

	processor.RunWorkers(ctx)

	router := processor.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("log-processor http listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	logger.Info("shutdown signal received", "signal", sig.String())

	cancel() // stops workers + subscribers
	processor.Wait()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	logger.Info("log-processor stopped cleanly")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}