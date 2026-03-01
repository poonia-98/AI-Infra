package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	Port              string
	DatabaseURL       string
	RedisURL          string
	NatsURL           string
	LeaseSeconds      int
	VisibilityTimeout int
	ClaimBatch        int
	MaxInflight       int
	RequeueInterval   time.Duration
}

func loadConfig() Config {
	lease := envInt("LEASE_SECONDS", 45)
	visibility := envInt("VISIBILITY_TIMEOUT_SECONDS", 120)
	claimBatch := envInt("CLAIM_BATCH", 20)
	maxInflight := envInt("MAX_INFLIGHT", 500)
	requeue := envDuration("REQUEUE_INTERVAL", 5*time.Second)
	return Config{
		Port:              env("PORT", "8096"),
		DatabaseURL:       env("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		RedisURL:          env("REDIS_URL", "redis://redis:6379"),
		NatsURL:           env("NATS_URL", "nats://nats:4222"),
		LeaseSeconds:      lease,
		VisibilityTimeout: visibility,
		ClaimBatch:        claimBatch,
		MaxInflight:       maxInflight,
		RequeueInterval:   requeue,
	}
}

var (
	queueEnqueued = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_execution_queue_enqueued_total", Help: "Total queue items enqueued"})
	queueClaimed = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_execution_queue_claimed_total", Help: "Total queue items claimed"})
	queueAcked = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_execution_queue_acked_total", Help: "Total queue items acked"})
	queueRetried = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_execution_queue_retried_total", Help: "Total queue items retried"})
	queueDeadLetter = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_execution_queue_dead_letter_total", Help: "Total queue items moved to dead letter"})
	queueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "platform_execution_queue_depth", Help: "Queue depth by status"}, []string{"status"})
)

func init() {
	prometheus.MustRegister(queueEnqueued, queueClaimed, queueAcked, queueRetried, queueDeadLetter, queueDepth)
}

type QueueItem struct {
	ID                  string                 `json:"id"`
	AgentID             string                 `json:"agent_id"`
	WorkflowExecutionID string                 `json:"workflow_exec_id,omitempty"`
	Priority            int                    `json:"priority"`
	Attempt             int                    `json:"attempt"`
	MaxAttempts         int                    `json:"max_attempts"`
	LeaseID             string                 `json:"lease_id,omitempty"`
	VisibilityTimeout   int                    `json:"visibility_timeout_seconds"`
	Input               map[string]interface{} `json:"input"`
}

type EnqueueRequest struct {
	OrganisationID      string                 `json:"organisation_id,omitempty"`
	AgentID             string                 `json:"agent_id" binding:"required"`
	WorkflowExecutionID string                 `json:"workflow_exec_id,omitempty"`
	QueueType           string                 `json:"queue_type,omitempty"`
	Priority            int                    `json:"priority"`
	MaxAttempts         int                    `json:"max_attempts"`
	DedupKey            string                 `json:"dedup_key,omitempty"`
	Input               map[string]interface{} `json:"input"`
	ScheduledAt         *time.Time             `json:"scheduled_at,omitempty"`
	VisibilityTimeout   int                    `json:"visibility_timeout_seconds"`
}

type ClaimRequest struct {
	WorkerID string `json:"worker_id" binding:"required"`
	Limit    int    `json:"limit"`
}

type AckRequest struct {
	WorkerID string `json:"worker_id" binding:"required"`
}

type FailRequest struct {
	WorkerID   string `json:"worker_id" binding:"required"`
	Error      string `json:"error" binding:"required"`
	RetryDelay int    `json:"retry_delay_seconds"`
}

type HeartbeatRequest struct {
	WorkerID string `json:"worker_id" binding:"required"`
}

type QueueService struct {
	cfg    Config
	db     *sql.DB
	redis  *redis.Client
	nc     *natsgo.Conn
	logger *slog.Logger
}

func NewQueueService(cfg Config, db *sql.DB, rc *redis.Client, nc *natsgo.Conn) *QueueService {
	return &QueueService{
		cfg:    cfg,
		db:     db,
		redis:  rc,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

func (s *QueueService) publish(event string, payload map[string]interface{}) {
	if s.nc == nil {
		return
	}
	payload["event"] = event
	payload["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(payload)
	_ = s.nc.Publish("execution.queue.events", b)
	_ = s.nc.Publish("execution.queue."+event, b)
}

func (s *QueueService) Enqueue(ctx context.Context, req EnqueueRequest) (string, bool, error) {
	if req.Priority <= 0 {
		req.Priority = 5
	}
	if req.Priority > 10 {
		req.Priority = 10
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = 3
	}
	if req.QueueType == "" {
		req.QueueType = "standard"
	}
	if req.VisibilityTimeout <= 0 {
		req.VisibilityTimeout = s.cfg.VisibilityTimeout
	}
	if req.Input == nil {
		req.Input = map[string]interface{}{}
	}
	scheduled := time.Now().UTC()
	if req.ScheduledAt != nil {
		scheduled = req.ScheduledAt.UTC()
	}

	if req.DedupKey != "" {
		existing, err := s.findActiveByDedup(ctx, req.DedupKey)
		if err == nil && existing != "" {
			return existing, true, nil
		}
		if s.redis != nil {
			ok, err := s.redis.SetNX(ctx, "queue:dedup:"+req.DedupKey, "1", 24*time.Hour).Result()
			if err == nil && !ok {
				existing, _ := s.findActiveByDedup(ctx, req.DedupKey)
				if existing != "" {
					return existing, true, nil
				}
				return "", true, nil
			}
		}
	}

	inputJSON, _ := json.Marshal(req.Input)
	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO execution_queues
		  (organisation_id, agent_id, workflow_exec_id, queue_type, priority, status,
		   input, attempt, max_attempts, scheduled_at, dedup_key, visibility_timeout_sec)
		VALUES
		  (NULLIF($1,'')::uuid, $2::uuid, NULLIF($3,'')::uuid, $4, $5, 'queued',
		   $6::jsonb, 1, $7, $8, NULLIF($9,''), $10)
		RETURNING id::text`,
		req.OrganisationID, req.AgentID, req.WorkflowExecutionID, req.QueueType, req.Priority,
		string(inputJSON), req.MaxAttempts, scheduled, req.DedupKey, req.VisibilityTimeout,
	).Scan(&id)
	if err != nil {
		return "", false, err
	}

	queueEnqueued.Inc()
	s.publish("enqueued", map[string]interface{}{"queue_id": id, "agent_id": req.AgentID, "priority": req.Priority})
	return id, false, nil
}

func (s *QueueService) findActiveByDedup(ctx context.Context, dedup string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text FROM execution_queues
		WHERE dedup_key=$1 AND status IN ('queued','leased','running')
		ORDER BY created_at DESC LIMIT 1`, dedup).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

func (s *QueueService) Claim(ctx context.Context, workerID string, limit int) ([]QueueItem, error) {
	if limit <= 0 {
		limit = s.cfg.ClaimBatch
	}
	if limit > 200 {
		limit = 200
	}

	var inflight int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_queues WHERE status IN ('leased','running')`).Scan(&inflight)
	if err != nil {
		return nil, err
	}
	if inflight >= s.cfg.MaxInflight {
		return []QueueItem{}, nil
	}
	if inflight+limit > s.cfg.MaxInflight {
		limit = s.cfg.MaxInflight - inflight
		if limit <= 0 {
			return []QueueItem{}, nil
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		WITH picked AS (
			SELECT id
			FROM execution_queues
			WHERE status='queued' AND scheduled_at<=NOW()
			ORDER BY priority ASC, scheduled_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		), leased AS (
			UPDATE execution_queues q
			SET status='leased',
				assigned_node=$1,
				locked_by=$1,
				locked_at=NOW(),
				lock_expires_at=NOW() + ($3 || ' seconds')::interval,
				lease_id=gen_random_uuid(),
				updated_at=NOW()
			FROM picked
			WHERE q.id=picked.id
			RETURNING q.id::text, q.agent_id::text, COALESCE(q.workflow_exec_id::text,''), q.input,
				q.priority, q.attempt, q.max_attempts, COALESCE(q.lease_id::text,''), q.visibility_timeout_sec
		)
		SELECT id, agent_id, workflow_exec_id, input, priority, attempt, max_attempts, lease_id, visibility_timeout_sec
		FROM leased`, workerID, limit, s.cfg.LeaseSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]QueueItem, 0, limit)
	for rows.Next() {
		var item QueueItem
		var raw []byte
		if err := rows.Scan(&item.ID, &item.AgentID, &item.WorkflowExecutionID, &raw, &item.Priority, &item.Attempt, &item.MaxAttempts, &item.LeaseID, &item.VisibilityTimeout); err != nil {
			return nil, err
		}
		item.Input = map[string]interface{}{}
		_ = json.Unmarshal(raw, &item.Input)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(items) > 0 {
		queueClaimed.Add(float64(len(items)))
		s.publish("claimed", map[string]interface{}{"worker_id": workerID, "count": len(items)})
	}
	return items, nil
}

func (s *QueueService) Ack(ctx context.Context, id, workerID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE execution_queues
		SET status='completed', completed_at=NOW(),
			locked_by=NULL, locked_at=NULL, lock_expires_at=NULL, lease_id=NULL,
			updated_at=NOW()
		WHERE id=$1::uuid AND status IN ('leased','running') AND locked_by=$2`, id, workerID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return errors.New("queue item not owned or not leasable")
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO billing_events (organisation_id, agent_id, event_type, quantity, unit, cost_usd, metadata)
		SELECT organisation_id, agent_id, 'execution.completed', 1, 'execution', 0,
			jsonb_build_object('queue_id', id::text, 'attempt', attempt)
		FROM execution_queues WHERE id=$1::uuid`, id)
	queueAcked.Inc()
	s.publish("acked", map[string]interface{}{"queue_id": id, "worker_id": workerID})
	return nil
}

func (s *QueueService) Fail(ctx context.Context, id, workerID, errText string, retryDelay int) error {
	if retryDelay < 0 {
		retryDelay = 0
	}
	var attempt, maxAttempts int
	err := s.db.QueryRowContext(ctx, `
		SELECT attempt, max_attempts
		FROM execution_queues
		WHERE id=$1::uuid AND locked_by=$2 AND status IN ('leased','running')`, id, workerID).Scan(&attempt, &maxAttempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("queue item not owned or not leasable")
		}
		return err
	}

	if attempt < maxAttempts {
		_, err = s.db.ExecContext(ctx, `
			UPDATE execution_queues
			SET status='queued', attempt=attempt+1, last_error=$2,
				scheduled_at=NOW() + ($3 || ' seconds')::interval,
				locked_by=NULL, locked_at=NULL, lock_expires_at=NULL, lease_id=NULL,
				updated_at=NOW()
			WHERE id=$1::uuid`, id, errText, retryDelay)
		if err != nil {
			return err
		}
		queueRetried.Inc()
		s.publish("retried", map[string]interface{}{"queue_id": id, "attempt": attempt + 1})
		return nil
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE execution_queues
		SET status='dead_letter', queue_type='dead_letter',
			last_error=$2, dead_letter_reason=$2, completed_at=NOW(),
			locked_by=NULL, locked_at=NULL, lock_expires_at=NULL, lease_id=NULL,
			updated_at=NOW()
		WHERE id=$1::uuid`, id, errText)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO billing_events (organisation_id, agent_id, event_type, quantity, unit, cost_usd, metadata)
		SELECT organisation_id, agent_id, 'execution.failed', 1, 'execution', 0,
			jsonb_build_object('queue_id', id::text, 'attempt', attempt, 'error', $2)
		FROM execution_queues WHERE id=$1::uuid`, id, errText)
	queueDeadLetter.Inc()
	s.publish("dead_letter", map[string]interface{}{"queue_id": id, "error": errText})
	return nil
}

func (s *QueueService) Heartbeat(ctx context.Context, id, workerID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE execution_queues
		SET lock_expires_at=NOW() + (visibility_timeout_sec || ' seconds')::interval,
			updated_at=NOW()
		WHERE id=$1::uuid AND locked_by=$2 AND status='leased'`, id, workerID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return errors.New("queue item not owned or not leased")
	}
	return nil
}

func (s *QueueService) RequeueExpired(ctx context.Context) {
	deadRows, err := s.db.QueryContext(ctx, `
		UPDATE execution_queues
		SET status='dead_letter', queue_type='dead_letter',
			dead_letter_reason=COALESCE(dead_letter_reason,'lease_expired_max_attempts'),
			completed_at=NOW(),
			locked_by=NULL, locked_at=NULL, lock_expires_at=NULL, lease_id=NULL,
			updated_at=NOW()
		WHERE status='leased' AND lock_expires_at < NOW() AND attempt >= max_attempts
		RETURNING id::text`)
	if err == nil {
		for deadRows.Next() {
			var id string
			_ = deadRows.Scan(&id)
			queueDeadLetter.Inc()
			s.publish("dead_letter", map[string]interface{}{"queue_id": id, "error": "lease_expired_max_attempts"})
		}
		deadRows.Close()
	}

	retryRows, err := s.db.QueryContext(ctx, `
		UPDATE execution_queues
		SET status='queued',
			attempt=attempt+1,
			last_error=COALESCE(last_error,'lease_expired'),
			scheduled_at=NOW(),
			locked_by=NULL, locked_at=NULL, lock_expires_at=NULL, lease_id=NULL,
			updated_at=NOW()
		WHERE status='leased' AND lock_expires_at < NOW() AND attempt < max_attempts
		RETURNING id::text`)
	if err == nil {
		count := 0
		for retryRows.Next() {
			var id string
			_ = retryRows.Scan(&id)
			count++
		}
		retryRows.Close()
		if count > 0 {
			queueRetried.Add(float64(count))
			s.publish("requeued_expired", map[string]interface{}{"count": count})
		}
	}
}

func (s *QueueService) UpdateDepth(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM execution_queues GROUP BY status`)
	if err != nil {
		return
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var status string
		var count float64
		if rows.Scan(&status, &count) == nil {
			queueDepth.WithLabelValues(status).Set(count)
			seen[status] = true
		}
	}
	for _, status := range []string{"queued", "leased", "running", "completed", "dead_letter", "failed"} {
		if !seen[status] {
			queueDepth.WithLabelValues(status).Set(0)
		}
	}
}

func (s *QueueService) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "execution-queue"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := s.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.POST("/queue/enqueue", func(c *gin.Context) {
		var req EnqueueRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		id, deduped, err := s.Enqueue(c.Request.Context(), req)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		status := 201
		if deduped {
			status = 200
		}
		c.JSON(status, gin.H{"id": id, "deduped": deduped})
	})

	r.POST("/queue/claim", func(c *gin.Context) {
		var req ClaimRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		items, err := s.Claim(c.Request.Context(), req.WorkerID, req.Limit)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"items": items})
	})

	r.POST("/queue/:id/ack", func(c *gin.Context) {
		var req AckRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := s.Ack(c.Request.Context(), c.Param("id"), req.WorkerID); err != nil {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "acked"})
	})

	r.POST("/queue/:id/fail", func(c *gin.Context) {
		var req FailRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := s.Fail(c.Request.Context(), c.Param("id"), req.WorkerID, req.Error, req.RetryDelay); err != nil {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "updated"})
	})

	r.POST("/queue/:id/heartbeat", func(c *gin.Context) {
		var req HeartbeatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := s.Heartbeat(c.Request.Context(), c.Param("id"), req.WorkerID); err != nil {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "extended"})
	})

	r.GET("/queue/stats", func(c *gin.Context) {
		rows, err := s.db.QueryContext(c.Request.Context(), `
			SELECT status, COUNT(*) FROM execution_queues GROUP BY status`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		stats := map[string]int{}
		for rows.Next() {
			var status string
			var count int
			if rows.Scan(&status, &count) == nil {
				stats[status] = count
			}
		}
		c.JSON(200, stats)
	})

	r.GET("/queue/dead-letter", func(c *gin.Context) {
		limit := envInt("DLQ_LIST_LIMIT", 100)
		rows, err := s.db.QueryContext(c.Request.Context(), `
			SELECT id::text, agent_id::text, priority, attempt, max_attempts,
				last_error, dead_letter_reason, completed_at
			FROM execution_queues
			WHERE status='dead_letter'
			ORDER BY completed_at DESC
			LIMIT $1`, limit)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, agentID string
			var priority, attempt, maxAttempts int
			var lastError, deadReason sql.NullString
			var completedAt sql.NullTime
			_ = rows.Scan(&id, &agentID, &priority, &attempt, &maxAttempts, &lastError, &deadReason, &completedAt)
			out = append(out, map[string]interface{}{
				"id": id,
				"agent_id": agentID,
				"priority": priority,
				"attempt": attempt,
				"max_attempts": maxAttempts,
				"last_error": nullString(lastError),
				"dead_letter_reason": nullString(deadReason),
				"completed_at": completedAt.Time,
			})
		}
		c.JSON(200, out)
	})

	r.POST("/queue/:id/requeue", func(c *gin.Context) {
		result, err := s.db.ExecContext(c.Request.Context(), `
			UPDATE execution_queues
			SET status='queued', queue_type='retry', attempt=1,
				locked_by=NULL, locked_at=NULL, lock_expires_at=NULL, lease_id=NULL,
				dead_letter_reason=NULL,
				scheduled_at=NOW(), updated_at=NOW()
			WHERE id=$1::uuid AND status='dead_letter'`, c.Param("id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			c.JSON(404, gin.H{"error": "not_found"})
			return
		}
		s.publish("requeued", map[string]interface{}{"queue_id": c.Param("id")})
		c.JSON(200, gin.H{"status": "queued"})
	})

	return r
}

func (s *QueueService) startMaintenance(ctx context.Context) {
	requeueTicker := time.NewTicker(s.cfg.RequeueInterval)
	metricsTicker := time.NewTicker(10 * time.Second)
	go func() {
		defer requeueTicker.Stop()
		defer metricsTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-requeueTicker.C:
				s.RequeueExpired(ctx)
			case <-metricsTicker.C:
				s.UpdateDepth(ctx)
			}
		}
	}()
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		logger.Error("database open failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)

	for i := 0; i < 30; i++ {
		if err = db.PingContext(context.Background()); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Error("database unavailable", "error", err)
		os.Exit(1)
	}

	rc := redis.NewClient(&redis.Options{Addr: redisAddr(cfg.RedisURL)})
	if pingErr := rc.Ping(context.Background()).Err(); pingErr != nil {
		logger.Warn("redis unavailable", "error", pingErr)
		rc = nil
	}

	var nc *natsgo.Conn
	for i := 0; i < 20; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second), natsgo.Name("execution-queue"))
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Warn("nats unavailable", "error", err)
		nc = nil
	}
	if nc != nil {
		defer nc.Close()
	}

	svc := NewQueueService(cfg, db, rc, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.startMaintenance(ctx)

	router := svc.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("execution-queue started", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

func redisAddr(redisURL string) string {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return "redis:6379"
	}
	return opt.Addr
}

func nullString(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := env(key, "")
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := env(key, "")
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

