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

type Config struct {
	DatabaseURL  string
	NatsURL      string
	Port         string
	SyncInterval time.Duration
}

func loadConfig() Config {
	interval, _ := time.ParseDuration(getEnv("SYNC_INTERVAL", "30s"))
	return Config{
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:      getEnv("NATS_URL", "nats://nats:4222"),
		Port:         getEnv("PORT", "8099"),
		SyncInterval: interval,
	}
}

var (
	clustersSynced    = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_clusters_synced_total"})
	failoverTriggered = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_region_failovers_total"})
	activeRegions     = prometheus.NewGauge(prometheus.GaugeOpts{Name: "platform_active_regions"})
)

func init() {
	prometheus.MustRegister(clustersSynced, failoverTriggered, activeRegions)
}

type ClusterController struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *ClusterController {
	return &ClusterController{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 10 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Region / Cluster Sync ──────────────────────────────────────────────────

func (cc *ClusterController) SyncAll(ctx context.Context) {
	rows, err := cc.db.QueryContext(ctx, `
		SELECT id::text, name, endpoint, api_key_hash FROM regions WHERE status='active'`)
	if err != nil {
		return
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var id, name, endpoint, apiKeyHash string
		rows.Scan(&id, &name, &endpoint, &apiKeyHash)
		go cc.syncRegion(ctx, id, name, endpoint, apiKeyHash)
		count++
	}
	activeRegions.Set(float64(count))
}

func (cc *ClusterController) syncRegion(ctx context.Context, regionID, name, endpoint, apiKeyHash string) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/api/v1/system/health", nil)
	if err != nil {
		cc.markRegionStatus(ctx, regionID, "degraded")
		return
	}
	req.Header.Set("X-API-Key", apiKeyHash)

	resp, err := cc.http.Do(req)
	if err != nil {
		cc.markRegionStatus(ctx, regionID, "offline")
		cc.checkFailover(ctx, regionID)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		cc.markRegionStatus(ctx, regionID, "degraded")
	} else {
		cc.markRegionStatus(ctx, regionID, "active")
	}

	// Sync cluster stats
	cc.syncClusterStats(ctx, regionID, endpoint, apiKeyHash)
	clustersSynced.Inc()
}

func (cc *ClusterController) syncClusterStats(ctx context.Context, regionID, endpoint, apiKeyHash string) {
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint+"/api/v1/nodes", nil)
	if req == nil {
		return
	}
	req.Header.Set("X-API-Key", apiKeyHash)
	resp, err := cc.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var nodes []struct {
		CPUTotal   int64 `json:"capacity"`
		MemoryMB   int64 `json:"memory_mb"`
		Status     string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&nodes)

	var cpuTotal, cpuAvail, memTotal, memAvail int64
	var nodeCount int
	for _, n := range nodes {
		if n.Status == "healthy" {
			nodeCount++
			cpuTotal += n.CPUTotal * 1000 // to millicores
		}
	}

	cc.db.ExecContext(ctx, `
		UPDATE clusters SET
		  node_count=$1, cpu_total_mc=$2, cpu_available_mc=$3,
		  memory_total_mb=$4, memory_available_mb=$5, last_sync_at=NOW()
		WHERE region_id=$6::uuid`,
		nodeCount, cpuTotal, cpuAvail, memTotal, memAvail, regionID)
}

func (cc *ClusterController) markRegionStatus(ctx context.Context, regionID, status string) {
	cc.db.ExecContext(ctx, `
		UPDATE regions SET status=$1, last_heartbeat=NOW(), updated_at=NOW()
		WHERE id=$2::uuid`, status, regionID)
	cc.nc.Publish("regions.status_changed", mustJSON(map[string]interface{}{
		"region_id": regionID, "status": status, "timestamp": time.Now().UTC(),
	}))
}

func (cc *ClusterController) checkFailover(ctx context.Context, failedRegionID string) {
	// Find agents placed in the failed region
	rows, err := cc.db.QueryContext(ctx, `
		SELECT agent_id::text, organisation_id::text FROM global_placements
		WHERE region_id=$1::uuid AND status='placed'`, failedRegionID)
	if err != nil {
		return
	}
	defer rows.Close()

	var agents [][2]string
	for rows.Next() {
		var agentID, orgID string
		rows.Scan(&agentID, &orgID)
		agents = append(agents, [2]string{agentID, orgID})
	}

	if len(agents) == 0 {
		return
	}

	// Find best alternative region
	var targetRegionID string
	cc.db.QueryRowContext(ctx, `
		SELECT id::text FROM regions
		WHERE status='active' AND id != $1::uuid
		ORDER BY node_count DESC LIMIT 1`, failedRegionID).Scan(&targetRegionID)

	if targetRegionID == "" {
		cc.logger.Error("no available region for failover", "failed_region", failedRegionID)
		return
	}

	// Record failover event
	var failoverID string
	cc.db.QueryRowContext(ctx, `
		INSERT INTO region_failover_events (from_region_id, to_region_id, trigger_reason, agents_migrated)
		VALUES ($1::uuid, $2::uuid, 'region_offline', $3)
		RETURNING id::text`,
		failedRegionID, targetRegionID, len(agents)).Scan(&failoverID)

	// Migrate placements
	for _, agent := range agents {
		cc.db.ExecContext(ctx, `
			UPDATE global_placements SET region_id=$1::uuid, status='migrating', updated_at=NOW()
			WHERE agent_id=$2::uuid`, targetRegionID, agent[0])

		cc.nc.Publish("global.agent.migrate", mustJSON(map[string]interface{}{
			"agent_id":          agent[0],
			"organisation_id":   agent[1],
			"from_region_id":    failedRegionID,
			"to_region_id":      targetRegionID,
			"failover_event_id": failoverID,
		}))
	}

	failoverTriggered.Inc()
	cc.logger.Info("failover initiated", "from_region", failedRegionID, "to_region", targetRegionID, "agents", len(agents))

	cc.db.ExecContext(ctx, `
		UPDATE region_failover_events SET status='completed', completed_at=NOW() WHERE id=$1::uuid`, failoverID)
}

// ── HTTP API ───────────────────────────────────────────────────────────────

func (cc *ClusterController) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "cluster-controller"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := cc.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// List regions
	r.GET("/regions", func(c *gin.Context) {
		rows, err := cc.db.QueryContext(c.Request.Context(), `
			SELECT id::text, name, display_name, provider, endpoint, status,
			       is_primary, node_count, last_heartbeat, created_at
			FROM regions ORDER BY is_primary DESC, name`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var regions []map[string]interface{}
		for rows.Next() {
			var id, name, dispName, provider, endpoint, status string
			var isPrimary bool
			var nodeCount int
			var lastHB, createdAt *time.Time
			rows.Scan(&id, &name, &dispName, &provider, &endpoint, &status, &isPrimary, &nodeCount, &lastHB, &createdAt)
			regions = append(regions, map[string]interface{}{
				"id": id, "name": name, "display_name": dispName,
				"provider": provider, "endpoint": endpoint, "status": status,
				"is_primary": isPrimary, "node_count": nodeCount,
				"last_heartbeat": lastHB, "created_at": createdAt,
			})
		}
		if regions == nil {
			regions = []map[string]interface{}{}
		}
		c.JSON(200, regions)
	})

	// Register a new region
	r.POST("/regions", func(c *gin.Context) {
		var body struct {
			Name        string `json:"name" binding:"required"`
			DisplayName string `json:"display_name"`
			Provider    string `json:"provider"`
			Endpoint    string `json:"endpoint" binding:"required"`
			APIKeyHash  string `json:"api_key_hash"`
			IsPrimary   bool   `json:"is_primary"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var id string
		err := cc.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO regions (name, display_name, provider, endpoint, api_key_hash, is_primary)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
			body.Name, body.DisplayName, body.Provider, body.Endpoint,
			body.APIKeyHash, body.IsPrimary).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id, "name": body.Name})
	})

	// Get clusters for a region
	r.GET("/regions/:region_id/clusters", func(c *gin.Context) {
		rows, err := cc.db.QueryContext(c.Request.Context(), `
			SELECT id::text, name, cluster_type, status, node_count,
			       cpu_total_mc, cpu_available_mc, memory_total_mb, last_sync_at
			FROM clusters WHERE region_id=$1::uuid`, c.Param("region_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var clusters []map[string]interface{}
		for rows.Next() {
			var id, name, clType, status string
			var nodeCount int
			var cpuTotal, cpuAvail, memTotal int64
			var lastSync *time.Time
			rows.Scan(&id, &name, &clType, &status, &nodeCount, &cpuTotal, &cpuAvail, &memTotal, &lastSync)
			clusters = append(clusters, map[string]interface{}{
				"id": id, "name": name, "cluster_type": clType, "status": status,
				"node_count": nodeCount, "cpu_total_mc": cpuTotal,
				"cpu_available_mc": cpuAvail, "memory_total_mb": memTotal, "last_sync_at": lastSync,
			})
		}
		if clusters == nil {
			clusters = []map[string]interface{}{}
		}
		c.JSON(200, clusters)
	})

	// Trigger region sync manually
	r.POST("/regions/:region_id/sync", func(c *gin.Context) {
		var id, name, endpoint, apiKeyHash string
		cc.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text, name, endpoint, COALESCE(api_key_hash,'')
			FROM regions WHERE id=$1::uuid`, c.Param("region_id")).
			Scan(&id, &name, &endpoint, &apiKeyHash)
		if id == "" {
			c.JSON(404, gin.H{"error": "region not found"})
			return
		}
		go cc.syncRegion(context.Background(), id, name, endpoint, apiKeyHash)
		c.JSON(202, gin.H{"status": "sync_triggered", "region_id": id})
	})

	// Get global agent placements
	r.GET("/placements", func(c *gin.Context) {
		rows, err := cc.db.QueryContext(c.Request.Context(), `
			SELECT gp.id::text, gp.agent_id::text, a.name,
			       gp.region_id::text, r.name as region_name,
			       gp.status, gp.placement_strategy, gp.placed_at
			FROM global_placements gp
			JOIN agents a ON a.id=gp.agent_id
			JOIN regions r ON r.id=gp.region_id
			ORDER BY gp.placed_at DESC LIMIT 200`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var placements []map[string]interface{}
		for rows.Next() {
			var id, agentID, agentName, regionID, regionName, status, strategy string
			var placedAt *time.Time
			rows.Scan(&id, &agentID, &agentName, &regionID, &regionName, &status, &strategy, &placedAt)
			placements = append(placements, map[string]interface{}{
				"id": id, "agent_id": agentID, "agent_name": agentName,
				"region_id": regionID, "region_name": regionName,
				"status": status, "placement_strategy": strategy, "placed_at": placedAt,
			})
		}
		if placements == nil {
			placements = []map[string]interface{}{}
		}
		c.JSON(200, placements)
	})

	// Force failover for a region
	r.POST("/regions/:region_id/failover", func(c *gin.Context) {
		go cc.checkFailover(context.Background(), c.Param("region_id"))
		c.JSON(202, gin.H{"status": "failover_triggered", "region_id": c.Param("region_id")})
	})

	return r
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting cluster-controller", "port", cfg.Port)

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
			natsgo.Name("cluster-controller"))
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

	cc := New(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sync loop
	go func() {
		ticker := time.NewTicker(cfg.SyncInterval)
		defer ticker.Stop()
		cc.SyncAll(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cc.SyncAll(ctx)
			}
		}
	}()

	router := cc.setupRouter()
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
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
func mustJSONStr(v interface{}) string { return string(mustJSON(v)) }

var _ = bytes.NewReader
var _ = fmt.Sprintf