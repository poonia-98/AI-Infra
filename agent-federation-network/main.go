package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"bytes"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL       string
	NatsURL           string
	Port              string
	NodeID            string
	FederationSecret  string
	HeartbeatInterval time.Duration
	TaskTimeoutSecs   int
}

func loadConfig() Config {
	hi, _ := time.ParseDuration(getEnv("HEARTBEAT_INTERVAL", "30s"))
	return Config{
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:           getEnv("NATS_URL", "nats://nats:4222"),
		Port:              getEnv("PORT", "8107"),
		NodeID:            getEnv("NODE_ID", "federation-default"),
		FederationSecret:  getEnv("FEDERATION_SECRET", "change-me-federation-secret"),
		HeartbeatInterval: hi,
		TaskTimeoutSecs:   300,
	}
}

// ── Prometheus ────────────────────────────────────────────────────────────────

var (
	federatedTasksSent = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_federation_tasks_sent_total"})
	federatedTasksRecv = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_federation_tasks_received_total"})
	activePeers        = prometheus.NewGauge(prometheus.GaugeOpts{Name: "platform_federation_active_peers"})
	taskLatency        = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "platform_federation_task_duration_seconds", Buckets: prometheus.DefBuckets})
)

func init() {
	prometheus.MustRegister(federatedTasksSent, federatedTasksRecv, activePeers, taskLatency)
}

// ── Domain Types ──────────────────────────────────────────────────────────────

type Peer struct {
	ID              string    `json:"id"`
	OrgID           string    `json:"org_id"`
	PeerName        string    `json:"peer_name"`
	EndpointURL     string    `json:"endpoint_url"`
	Status          string    `json:"status"` // active | inactive | banned
	TrustLevel      string    `json:"trust_level"` // full | partial | sandboxed
	PublicKeyHash   string    `json:"public_key_hash,omitempty"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type FederatedTask struct {
	ID             string                 `json:"id"`
	SourceOrgID    string                 `json:"source_org_id"`
	TargetPeerID   string                 `json:"target_peer_id"`
	LocalAgentID   string                 `json:"local_agent_id"`
	RemoteAgentID  string                 `json:"remote_agent_id,omitempty"`
	TaskType       string                 `json:"task_type"`
	Payload        map[string]interface{} `json:"payload"`
	Result         map[string]interface{} `json:"result,omitempty"`
	Status         string                 `json:"status"` // pending | dispatched | running | completed | failed
	TrustLevel     string                 `json:"trust_level"`
	CostBudgetUSD  float64               `json:"cost_budget_usd"`
	ActualCostUSD  float64               `json:"actual_cost_usd"`
	ExpiresAt      *time.Time            `json:"expires_at"`
	StartedAt      *time.Time            `json:"started_at"`
	CompletedAt    *time.Time            `json:"completed_at"`
	ErrorMessage   string                `json:"error_message,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
}

type IncomingTask struct {
	TaskID        string                 `json:"task_id" binding:"required"`
	SourceOrgSig  string                 `json:"source_org_sig" binding:"required"` // HMAC-SHA256 signature
	TaskType      string                 `json:"task_type" binding:"required"`
	Payload       map[string]interface{} `json:"payload"`
	CostBudgetUSD float64               `json:"cost_budget_usd"`
	CallbackURL   string                 `json:"callback_url"`
}

// ── Federation Network ────────────────────────────────────────────────────────

type FederationNetwork struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger

	mu        sync.RWMutex
	peerCache map[string]*Peer // peerID → Peer
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *FederationNetwork {
	fn := &FederationNetwork{
		cfg:       cfg,
		db:        db,
		nc:        nc,
		logger:    slog.Default(),
		peerCache: make(map[string]*Peer),
	}
	return fn
}

// ── Peer Management ──────────────────────────────────────────────────────────

func (fn *FederationNetwork) loadPeers() {
	rows, err := fn.db.Query(`SELECT id, org_id, peer_name, endpoint_url, status, trust_level, last_seen_at, created_at, updated_at FROM federation_peers WHERE status != 'banned'`)
	if err != nil { return }
	defer rows.Close()
	fn.mu.Lock()
	defer fn.mu.Unlock()
	for rows.Next() {
		p := &Peer{}
		rows.Scan(&p.ID, &p.OrgID, &p.PeerName, &p.EndpointURL, &p.Status, &p.TrustLevel, &p.LastSeenAt, &p.CreatedAt, &p.UpdatedAt)
		fn.peerCache[p.ID] = p
	}
	activePeers.Set(float64(len(fn.peerCache)))
}

func (fn *FederationNetwork) signPayload(payload []byte) string {
	mac := hmac.New(sha256.New, []byte(fn.cfg.FederationSecret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func (fn *FederationNetwork) verifySignature(payload []byte, sig string) bool {
	expected := fn.signPayload(payload)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── Task Dispatch ─────────────────────────────────────────────────────────────

func (fn *FederationNetwork) SendTask(ctx context.Context, peerID, localAgentID string, payload map[string]interface{}) (string, error) {
	fn.mu.RLock()
	peer, ok := fn.peerCache[peerID]
	fn.mu.RUnlock()
	if !ok {
		// Try DB
		peer = &Peer{}
		err := fn.db.QueryRowContext(ctx, `SELECT id, endpoint_url, status, trust_level FROM federation_peers WHERE id=$1`, peerID).
			Scan(&peer.ID, &peer.EndpointURL, &peer.Status, &peer.TrustLevel)
		if err != nil { return "", fmt.Errorf("peer not found: %s", peerID) }
	}
	if peer.Status != "active" {
		return "", fmt.Errorf("peer %s is not active (status: %s)", peerID, peer.Status)
	}

	taskID := generateID()
	payloadBytes, _ := json.Marshal(payload)
	sig := fn.signPayload(payloadBytes)

	task := IncomingTask{
		TaskID:       taskID,
		SourceOrgSig: sig,
		TaskType:     "agent_delegation",
		Payload:      payload,
	}
	taskBytes, _ := json.Marshal(task)

	start := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(fn.cfg.TaskTimeoutSecs)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", peer.EndpointURL+"/federation/incoming", bytes.NewReader(taskBytes))
	if err != nil { return "", err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Federation-Node", fn.cfg.NodeID)

	resp, err := http.DefaultClient.Do(req)
	taskLatency.Observe(time.Since(start).Seconds())
	if err != nil { return "", fmt.Errorf("dispatch failed: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 400 { return "", fmt.Errorf("peer returned HTTP %d", resp.StatusCode) }

	// Record in DB
	fn.db.ExecContext(ctx, `
		INSERT INTO federation_tasks (id, source_org_id, target_peer_id, local_agent_id, task_type, payload, status, trust_level)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'dispatched', $7)`,
		taskID, "", peerID, localAgentID, "agent_delegation", string(payloadBytes), peer.TrustLevel)

	federatedTasksSent.Inc()

	// NATS event
	fn.nc.Publish("agents.events", mustJSON(map[string]interface{}{
		"event_type": "federation.task_dispatched",
		"task_id":    taskID,
		"peer_id":    peerID,
		"source":     "federation-network",
	}))

	return taskID, nil
}

// ── Peer Heartbeat ────────────────────────────────────────────────────────────

func (fn *FederationNetwork) StartPeerHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(fn.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn.pingAllPeers(ctx)
		}
	}
}

func (fn *FederationNetwork) pingAllPeers(ctx context.Context) {
	fn.mu.RLock()
	peers := make([]*Peer, 0, len(fn.peerCache))
	for _, p := range fn.peerCache {
		peers = append(peers, p)
	}
	fn.mu.RUnlock()

	now := time.Now()
	for _, peer := range peers {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(pingCtx, "GET", peer.EndpointURL+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		newStatus := "active"
		if err != nil || resp.StatusCode >= 400 {
			newStatus = "inactive"
		}
		fn.db.Exec(`UPDATE federation_peers SET status=$1, last_seen_at=$2 WHERE id=$3`, newStatus, now, peer.ID)
		if peer.Status != newStatus {
			fn.mu.Lock()
			if p, ok := fn.peerCache[peer.ID]; ok {
				p.Status = newStatus
				p.LastSeenAt = &now
			}
			fn.mu.Unlock()
		}
	}
	activePeers.Set(float64(len(peers)))
}

// ── HTTP Router ───────────────────────────────────────────────────────────────

func (fn *FederationNetwork) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "agent-federation-network", "node_id": fn.cfg.NodeID})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := fn.db.Ping(); err != nil {
			c.JSON(503, gin.H{"ready": false, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ready": true})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	api := r.Group("/api/v1")

	// ── Peers ────────────────────────────────────────────────────────────────
	api.GET("/federation/peers", func(c *gin.Context) {
		rows, err := fn.db.QueryContext(c.Request.Context(),
			`SELECT id, org_id, peer_name, endpoint_url, status, trust_level, last_seen_at, created_at, updated_at FROM federation_peers ORDER BY created_at DESC LIMIT 100`)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var peers []Peer
		for rows.Next() {
			var p Peer
			rows.Scan(&p.ID, &p.OrgID, &p.PeerName, &p.EndpointURL, &p.Status, &p.TrustLevel, &p.LastSeenAt, &p.CreatedAt, &p.UpdatedAt)
			peers = append(peers, p)
		}
		if peers == nil { peers = []Peer{} }
		c.JSON(200, peers)
	})

	api.POST("/federation/peers", func(c *gin.Context) {
		var body struct {
			OrgID       string `json:"org_id"`
			PeerName    string `json:"peer_name" binding:"required"`
			EndpointURL string `json:"endpoint_url" binding:"required"`
			TrustLevel  string `json:"trust_level"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.TrustLevel == "" { body.TrustLevel = "sandboxed" }
		id := generateID()
		_, err := fn.db.ExecContext(c.Request.Context(),
			`INSERT INTO federation_peers (id, org_id, peer_name, endpoint_url, status, trust_level) VALUES ($1, $2::uuid, $3, $4, 'active', $5)`,
			id, nilIfEmpty(body.OrgID), body.PeerName, body.EndpointURL, body.TrustLevel)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		fn.loadPeers()
		c.JSON(201, gin.H{"id": id, "peer_name": body.PeerName, "trust_level": body.TrustLevel})
	})

	api.GET("/federation/peers/:id", func(c *gin.Context) {
		var p Peer
		err := fn.db.QueryRowContext(c.Request.Context(),
			`SELECT id, org_id, peer_name, endpoint_url, status, trust_level, last_seen_at, created_at, updated_at FROM federation_peers WHERE id=$1`, c.Param("id")).
			Scan(&p.ID, &p.OrgID, &p.PeerName, &p.EndpointURL, &p.Status, &p.TrustLevel, &p.LastSeenAt, &p.CreatedAt, &p.UpdatedAt)
		if err != nil { c.JSON(404, gin.H{"error": "peer not found"}); return }
		c.JSON(200, p)
	})

	api.PATCH("/federation/peers/:id", func(c *gin.Context) {
		var body struct {
			TrustLevel  string `json:"trust_level"`
			Status      string `json:"status"`
			EndpointURL string `json:"endpoint_url"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		fn.db.ExecContext(c.Request.Context(),
			`UPDATE federation_peers SET trust_level=COALESCE(NULLIF($1,''), trust_level), status=COALESCE(NULLIF($2,''), status), endpoint_url=COALESCE(NULLIF($3,''), endpoint_url), updated_at=NOW() WHERE id=$4`,
			body.TrustLevel, body.Status, body.EndpointURL, c.Param("id"))
		fn.loadPeers()
		c.JSON(200, gin.H{"updated": true})
	})

	api.DELETE("/federation/peers/:id", func(c *gin.Context) {
		fn.db.ExecContext(c.Request.Context(), `UPDATE federation_peers SET status='banned' WHERE id=$1`, c.Param("id"))
		fn.mu.Lock()
		delete(fn.peerCache, c.Param("id"))
		fn.mu.Unlock()
		activePeers.Set(float64(len(fn.peerCache)))
		c.JSON(200, gin.H{"removed": true})
	})

	// ── Tasks ────────────────────────────────────────────────────────────────
	api.POST("/federation/peers/:id/dispatch", func(c *gin.Context) {
		var body struct {
			LocalAgentID  string                 `json:"local_agent_id" binding:"required"`
			TaskType      string                 `json:"task_type"`
			Payload       map[string]interface{} `json:"payload"`
			CostBudgetUSD float64               `json:"cost_budget_usd"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		taskID, err := fn.SendTask(c.Request.Context(), c.Param("id"), body.LocalAgentID, body.Payload)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(201, gin.H{"task_id": taskID, "status": "dispatched"})
	})

	api.GET("/federation/tasks", func(c *gin.Context) {
		limit := queryInt(c, "limit", 50)
		rows, err := fn.db.QueryContext(c.Request.Context(),
			`SELECT id, source_org_id, target_peer_id, local_agent_id, task_type, status, trust_level, actual_cost_usd, created_at FROM federation_tasks ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var tasks []FederatedTask
		for rows.Next() {
			var t FederatedTask
			rows.Scan(&t.ID, &t.SourceOrgID, &t.TargetPeerID, &t.LocalAgentID, &t.TaskType, &t.Status, &t.TrustLevel, &t.ActualCostUSD, &t.CreatedAt)
			tasks = append(tasks, t)
		}
		if tasks == nil { tasks = []FederatedTask{} }
		c.JSON(200, tasks)
	})

	api.GET("/federation/tasks/:id", func(c *gin.Context) {
		var t FederatedTask
		err := fn.db.QueryRowContext(c.Request.Context(),
			`SELECT id, source_org_id, target_peer_id, local_agent_id, task_type, payload, result, status, trust_level, actual_cost_usd, error_message, created_at FROM federation_tasks WHERE id=$1`,
			c.Param("id")).Scan(&t.ID, &t.SourceOrgID, &t.TargetPeerID, &t.LocalAgentID, &t.TaskType, &t.Payload, &t.Result, &t.Status, &t.TrustLevel, &t.ActualCostUSD, &t.ErrorMessage, &t.CreatedAt)
		if err != nil { c.JSON(404, gin.H{"error": "task not found"}); return }
		c.JSON(200, t)
	})

	// ── Incoming federation tasks (from remote peers) ────────────────────────
	r.POST("/federation/incoming", func(c *gin.Context) {
		var task IncomingTask
		if err := c.ShouldBindJSON(&task); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		payloadBytes, _ := json.Marshal(task.Payload)
		if !fn.verifySignature(payloadBytes, task.SourceOrgSig) {
			c.JSON(403, gin.H{"error": "invalid signature"})
			return
		}
		fn.db.ExecContext(c.Request.Context(),
			`INSERT INTO federation_tasks (id, task_type, payload, status, trust_level) VALUES ($1, $2, $3::jsonb, 'received', 'sandboxed') ON CONFLICT DO NOTHING`,
			task.TaskID, task.TaskType, string(payloadBytes))
		federatedTasksRecv.Inc()
		fn.nc.Publish("agents.events", mustJSON(map[string]interface{}{
			"event_type": "federation.task_received",
			"task_id":    task.TaskID,
			"source":     "federation-network",
		}))
		c.JSON(202, gin.H{"task_id": task.TaskID, "accepted": true})
	})

	// ── Network stats ────────────────────────────────────────────────────────
	api.GET("/federation/stats", func(c *gin.Context) {
		var totalPeers, activePeerCount, totalTasksSent, totalTasksRecv int
		fn.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) FROM federation_peers`).Scan(&totalPeers)
		fn.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) FROM federation_peers WHERE status='active'`).Scan(&activePeerCount)
		fn.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) FROM federation_tasks WHERE source_org_id != ''`).Scan(&totalTasksSent)
		fn.db.QueryRowContext(c.Request.Context(), `SELECT COUNT(*) FROM federation_tasks WHERE source_org_id = ''`).Scan(&totalTasksRecv)
		c.JSON(200, gin.H{
			"total_peers":       totalPeers,
			"active_peers":      activePeerCount,
			"tasks_sent_total":  totalTasksSent,
			"tasks_recv_total":  totalTasksRecv,
			"node_id":           fn.cfg.NodeID,
		})
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg := loadConfig()

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil { logger.Error("db open", "error", err); os.Exit(1) }
	defer db.Close()
	for i := 0; i < 10; i++ {
		if err = db.Ping(); err == nil { break }
		logger.Info("waiting for postgres", "attempt", i+1)
		time.Sleep(3 * time.Second)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)

	nc, err := natsgo.Connect(cfg.NatsURL, natsgo.RetryOnFailedConnect(true), natsgo.MaxReconnects(-1))
	if err != nil { logger.Warn("nats connect failed", "error", err) }
	defer func() { if nc != nil { nc.Close() } }()

	fn := New(cfg, db, nc)
	fn.loadPeers()
	logger.Info("federation-network started", "node_id", cfg.NodeID, "peers_loaded", len(fn.peerCache))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fn.StartPeerHeartbeat(ctx)

	router := fn.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

func queryInt(c *gin.Context, key string, def int) int {
	v := c.Query(key)
	if v == "" { return def }
	var n int
	fmt.Sscanf(v, "%d", &n)
	if n <= 0 { return def }
	return n
}

func nilIfEmpty(s string) interface{} {
	if s == "" { return nil }
	return s
}


	NewReader: func(b []byte) *bytesReader { return &bytesReader{b: b, i: 0} },
}



	if r.i >= len(r.b) { return 0, fmt.Errorf("EOF") }
	n = copy(p, r.b[r.i:])
	r.i += n
	return
}