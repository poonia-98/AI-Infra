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

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL      string
	NatsURL          string
	Port             string
	HeartbeatTTL     time.Duration // node dead threshold
	ReassignInterval time.Duration // how often to check for dead nodes
	HeartbeatRetain  time.Duration // how long to keep heartbeat rows
}

func loadConfig() Config {
	hbTTL, _ := time.ParseDuration(getEnv("HEARTBEAT_TTL", "30s"))
	reassign, _ := time.ParseDuration(getEnv("REASSIGN_INTERVAL", "15s"))
	retain, _ := time.ParseDuration(getEnv("HEARTBEAT_RETAIN", "168h")) // 7 days
	return Config{
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:          getEnv("NATS_URL", "nats://nats:4222"),
		Port:             getEnv("PORT", "8087"),
		HeartbeatTTL:     hbTTL,
		ReassignInterval: reassign,
		HeartbeatRetain:  retain,
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────────

var (
	nodeCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_nodes_total",
		Help: "Number of executor nodes by status",
	}, []string{"status"})

	heartbeatLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "platform_heartbeat_latency_seconds",
		Help:    "Time between expected and received heartbeats",
		Buckets: prometheus.DefBuckets,
	}, []string{"node_id"})

	reassignmentTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_workload_reassignments_total",
		Help: "Total workload reassignments after node failure",
	}, []string{"reason"})
)

func init() {
	prometheus.MustRegister(nodeCount, heartbeatLatency, reassignmentTotal)
}

// ── Domain ────────────────────────────────────────────────────────────────────

type Node struct {
	ID            string
	NodeID        string
	Hostname      string
	Address       string
	Port          int
	Status        string
	Capacity      int
	CurrentLoad   int
	LastHeartbeat time.Time
	Metadata      map[string]interface{}
}

type HeartbeatRequest struct {
	NodeID     string                 `json:"node_id" binding:"required"`
	Hostname   string                 `json:"hostname"`
	Address    string                 `json:"address"`
	Port       int                    `json:"port"`
	Capacity   int                    `json:"capacity"`
	Load       int                    `json:"load"`
	CPUPercent float64                `json:"cpu_percent"`
	MemoryMB   float64                `json:"memory_mb"`
	Metadata   map[string]interface{} `json:"metadata"`
}

// ── Node Manager ──────────────────────────────────────────────────────────────

type NodeManager struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
	mu     sync.RWMutex
}

func NewNodeManager(cfg Config, db *sql.DB, nc *natsgo.Conn) *NodeManager {
	return &NodeManager{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Heartbeat handler ─────────────────────────────────────────────────────────

func (m *NodeManager) ProcessHeartbeat(ctx context.Context, req HeartbeatRequest) error {
	now := time.Now().UTC()

	meta, _ := json.Marshal(req.Metadata)
	if string(meta) == "null" {
		meta = []byte("{}")
	}

	// Upsert executor_nodes
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO executor_nodes
		  (node_id, hostname, address, port, status, capacity, current_load, last_heartbeat, metadata)
		VALUES ($1, $2, $3, $4, 'healthy', $5, $6, $7, $8::jsonb)
		ON CONFLICT (node_id) DO UPDATE
		  SET hostname       = EXCLUDED.hostname,
		      address        = EXCLUDED.address,
		      port           = EXCLUDED.port,
		      status         = 'healthy',
		      capacity       = EXCLUDED.capacity,
		      current_load   = EXCLUDED.current_load,
		      last_heartbeat = EXCLUDED.last_heartbeat,
		      metadata       = EXCLUDED.metadata,
		      updated_at     = NOW()`,
		req.NodeID, req.Hostname, req.Address, req.Port,
		req.Capacity, req.Load, now, string(meta))
	if err != nil {
		return fmt.Errorf("upsert executor_nodes: %w", err)
	}

	// Append to node_heartbeats (time-series)
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO node_heartbeats
		  (node_id, status, cpu_percent, memory_mb, load, capacity, metadata, received_at)
		VALUES ($1, 'healthy', $2, $3, $4, $5, $6::jsonb, $7)`,
		req.NodeID, req.CPUPercent, req.MemoryMB, req.Load, req.Capacity, string(meta), now)
	if err != nil {
		m.logger.Warn("failed to insert heartbeat record", "error", err, "node_id", req.NodeID)
	}

	m.publishEvent("platform.node.heartbeat", map[string]interface{}{
		"node_id":   req.NodeID,
		"status":    "healthy",
		"load":      req.Load,
		"capacity":  req.Capacity,
		"timestamp": now,
	})

	return nil
}

// ── Dead-node detection loop ──────────────────────────────────────────────────

func (m *NodeManager) RunDeadNodeDetector(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.ReassignInterval)
	defer ticker.Stop()
	m.logger.Info("dead-node detector started", "interval", m.cfg.ReassignInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.detectDeadNodes(ctx)
			m.updateNodeMetrics(ctx)
			m.pruneOldHeartbeats(ctx)
		}
	}
}

func (m *NodeManager) detectDeadNodes(ctx context.Context) {
	ttlSecs := m.cfg.HeartbeatTTL.Seconds()

	// Find nodes that haven't sent a heartbeat within TTL
	rows, err := m.db.QueryContext(ctx, `
		SELECT node_id, hostname, last_heartbeat, status
		FROM executor_nodes
		WHERE status != 'dead'
		  AND NOW() - last_heartbeat > ($1 * INTERVAL '1 second')`,
		ttlSecs)
	if err != nil {
		m.logger.Error("dead-node query failed", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var nodeID, hostname, status string
		var lastHB time.Time
		if err := rows.Scan(&nodeID, &hostname, &lastHB, &status); err != nil {
			continue
		}
		m.logger.Warn("dead node detected",
			"node_id", nodeID, "hostname", hostname,
			"last_heartbeat", lastHB, "age", time.Since(lastHB))
		m.markNodeDead(ctx, nodeID)
		m.reassignWorkloads(ctx, nodeID)
	}
}

func (m *NodeManager) markNodeDead(ctx context.Context, nodeID string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE executor_nodes SET status='dead', updated_at=NOW() WHERE node_id=$1 AND status!='dead'`,
		nodeID)
	if err != nil {
		m.logger.Error("failed to mark node dead", "error", err, "node_id", nodeID)
		return
	}
	m.publishEvent("platform.node.dead", map[string]interface{}{
		"node_id": nodeID, "timestamp": time.Now().UTC(),
	})
}

func (m *NodeManager) reassignWorkloads(ctx context.Context, deadNodeID string) {
	// Find healthy node with most available capacity
	var targetNodeID sql.NullString
	err := m.db.QueryRowContext(ctx, `
		SELECT node_id
		FROM executor_nodes
		WHERE status='healthy'
		  AND capacity > current_load
		ORDER BY (capacity - current_load) DESC
		LIMIT 1`).Scan(&targetNodeID)
	if err != nil || !targetNodeID.Valid {
		m.logger.Warn("no healthy node available for reassignment", "dead_node", deadNodeID)
		return
	}

	// Mark running executions on dead node as failed (reconciliation will restart based on policy)
	res, err := m.db.ExecContext(ctx, `
		UPDATE executions
		SET status='failed', actual_state='failed',
		    error='node dead during execution', completed_at=NOW()
		WHERE node_id=$1 AND status IN ('running','starting')`,
		deadNodeID)
	if err != nil {
		m.logger.Error("failed to fail executions on dead node", "error", err)
		return
	}

	rows, _ := res.RowsAffected()
	if rows > 0 {
		reassignmentTotal.With(prometheus.Labels{"reason": "node_dead"}).Add(float64(rows))
		m.logger.Info("reassigned workloads from dead node",
			"dead_node", deadNodeID, "target_node", targetNodeID.String,
			"executions_failed", rows)

		m.publishEvent("platform.node.workloads_reassigned", map[string]interface{}{
			"dead_node":           deadNodeID,
			"target_node":         targetNodeID.String,
			"executions_affected": rows,
			"timestamp":           time.Now().UTC(),
		})
	}
}

func (m *NodeManager) updateNodeMetrics(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM executor_nodes GROUP BY status`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count float64
		if err := rows.Scan(&status, &count); err == nil {
			nodeCount.With(prometheus.Labels{"status": status}).Set(count)
		}
	}
}

func (m *NodeManager) pruneOldHeartbeats(ctx context.Context) {
	cutoff := time.Now().Add(-m.cfg.HeartbeatRetain)
	res, _ := m.db.ExecContext(ctx,
		`DELETE FROM node_heartbeats WHERE received_at < $1`, cutoff)
	if n, _ := res.RowsAffected(); n > 0 {
		m.logger.Debug("pruned old heartbeat records", "count", n)
	}
}

// ── Capacity-aware node selection ─────────────────────────────────────────────

func (m *NodeManager) SelectNode(ctx context.Context) (string, error) {
	var nodeID string
	err := m.db.QueryRowContext(ctx, `
		SELECT node_id
		FROM executor_nodes
		WHERE status='healthy'
		  AND capacity > current_load
		ORDER BY (capacity - current_load) DESC
		LIMIT 1`).Scan(&nodeID)
	if err != nil {
		return "", fmt.Errorf("no healthy node available: %w", err)
	}
	return nodeID, nil
}

// ── NATS publisher ────────────────────────────────────────────────────────────

func (m *NodeManager) publishEvent(subject string, data map[string]interface{}) {
	if m.nc == nil {
		return
	}
	payload, _ := json.Marshal(data)
	_ = m.nc.Publish(subject, payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (m *NodeManager) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// ── Health endpoints ───────────────────────────────────────────────
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "node-manager"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := m.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready", "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// ── Node registration / heartbeat ──────────────────────────────────
	r.POST("/nodes/heartbeat", func(c *gin.Context) {
		var req HeartbeatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.Port == 0 {
			req.Port = 8081
		}
		if req.Capacity == 0 {
			req.Capacity = 50
		}
		if err := m.ProcessHeartbeat(c.Request.Context(), req); err != nil {
			m.logger.Error("heartbeat processing failed", "error", err)
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "ok", "node_id": req.NodeID})
	})

	// ── Node listing ───────────────────────────────────────────────────
	r.GET("/nodes", func(c *gin.Context) {
		status := c.Query("status") // optional filter
		var rows *sql.Rows
		var err error
		if status != "" {
			rows, err = m.db.QueryContext(c.Request.Context(), `
				SELECT id::text, node_id, hostname, address, port, status,
				       capacity, current_load, last_heartbeat
				FROM executor_nodes WHERE status=$1
				ORDER BY last_heartbeat DESC`, status)
		} else {
			rows, err = m.db.QueryContext(c.Request.Context(), `
				SELECT id::text, node_id, hostname, address, port, status,
				       capacity, current_load, last_heartbeat
				FROM executor_nodes ORDER BY last_heartbeat DESC`)
		}
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		nodes := []map[string]interface{}{}
		for rows.Next() {
			var id, nodeID, hostname, address, stat string
			var port, capacity, load int
			var hb time.Time
			if err := rows.Scan(&id, &nodeID, &hostname, &address, &port,
				&stat, &capacity, &load, &hb); err != nil {
				continue
			}
			nodes = append(nodes, map[string]interface{}{
				"id": id, "node_id": nodeID, "hostname": hostname,
				"address": address, "port": port, "status": stat,
				"capacity": capacity, "current_load": load,
				"last_heartbeat": hb, "available": capacity - load,
				"utilization_pct": float64(load) / float64(max(capacity, 1)) * 100,
			})
		}
		c.JSON(200, nodes)
	})

	// ── Node heartbeat history ─────────────────────────────────────────
	r.GET("/nodes/:node_id/heartbeats", func(c *gin.Context) {
		nodeID := c.Param("node_id")
		limit := 100
		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT cpu_percent, memory_mb, load, capacity, received_at
			FROM node_heartbeats
			WHERE node_id=$1
			ORDER BY received_at DESC LIMIT $2`, nodeID, limit)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var hbs []map[string]interface{}
		for rows.Next() {
			var cpu, mem float64
			var load, capacity int
			var at time.Time
			if err := rows.Scan(&cpu, &mem, &load, &capacity, &at); err != nil {
				continue
			}
			hbs = append(hbs, map[string]interface{}{
				"cpu_percent": cpu, "memory_mb": mem,
				"load": load, "capacity": capacity, "received_at": at,
			})
		}
		if hbs == nil {
			hbs = []map[string]interface{}{}
		}
		c.JSON(200, hbs)
	})

	// ── Capacity-aware node selection ──────────────────────────────────
	r.GET("/nodes/select", func(c *gin.Context) {
		nodeID, err := m.SelectNode(c.Request.Context())
		if err != nil {
			c.JSON(503, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"node_id": nodeID})
	})

	// ── Drain a node (admin) ───────────────────────────────────────────
	r.POST("/nodes/:node_id/drain", func(c *gin.Context) {
		nodeID := c.Param("node_id")
		_, err := m.db.ExecContext(c.Request.Context(),
			`UPDATE executor_nodes SET status='draining', updated_at=NOW() WHERE node_id=$1`, nodeID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		m.publishEvent("platform.node.draining", map[string]interface{}{
			"node_id": nodeID, "timestamp": time.Now().UTC(),
		})
		c.JSON(200, gin.H{"node_id": nodeID, "status": "draining"})
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting node-manager", "port", cfg.Port)

	// ── PostgreSQL ─────────────────────────────────────────────────────
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
	db.SetConnMaxLifetime(5 * time.Minute)
	defer db.Close()

	// ── NATS ───────────────────────────────────────────────────────────
	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL,
			natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second),
			natsgo.Name("node-manager"))
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
	logger.Info("nats connected")

	mgr := NewNodeManager(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mgr.RunDeadNodeDetector(ctx)

	router := mgr.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("node-manager http listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	// ── Graceful shutdown ──────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	logger.Info("shutdown signal received", "signal", sig.String())
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	logger.Info("node-manager stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}