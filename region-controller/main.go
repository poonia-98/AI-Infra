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

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL        string
	NatsURL            string
	Port               string
	RegionName         string
	RegionProvider     string
	GlobalSchedulerURL string
	SyncInterval       time.Duration
	FailoverThreshold  time.Duration
}

func loadConfig() Config {
	si, _ := time.ParseDuration(getEnv("SYNC_INTERVAL", "30s"))
	ft, _ := time.ParseDuration(getEnv("FAILOVER_THRESHOLD", "2m"))
	return Config{
		DatabaseURL:        getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:            getEnv("NATS_URL", "nats://nats:4222"),
		Port:               getEnv("PORT", "8099"),
		RegionName:         getEnv("REGION_NAME", "local"),
		RegionProvider:     getEnv("REGION_PROVIDER", "docker"),
		GlobalSchedulerURL: getEnv("GLOBAL_SCHEDULER_URL", "http://global-scheduler:8098"),
		SyncInterval:       si,
		FailoverThreshold:  ft,
	}
}

// ── Prometheus ────────────────────────────────────────────────────────────────

var (
	agentsInRegion   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "platform_region_agents"}, []string{"region"})
	failoverEvents   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "platform_region_failovers_total"}, []string{"from_region", "to_region"})
	placementLatency = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "platform_region_placement_seconds", Buckets: prometheus.DefBuckets})
)

func init() {
	prometheus.MustRegister(agentsInRegion, failoverEvents, placementLatency)
}

// ── Types ─────────────────────────────────────────────────────────────────────

type Region struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	DisplayName  string    `json:"display_name"`
	Provider     string    `json:"provider"`
	Status       string    `json:"status"`
	IsPrimary    bool      `json:"is_primary"`
	EndpointURL  string    `json:"endpoint_url,omitempty"`
	AgentCount   int       `json:"agent_count"`
	ClusterCount int       `json:"cluster_count"`
	Metadata     any       `json:"metadata,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type RegionPlacement struct {
	ID            string    `json:"id"`
	AgentID       string    `json:"agent_id"`
	RegionID      string    `json:"region_id"`
	RegionName    string    `json:"region_name"`
	Status        string    `json:"status"`
	PlacementType string    `json:"placement_type"`
	CreatedAt     time.Time `json:"created_at"`
}

type FailoverEvent struct {
	ID           string    `json:"id"`
	AgentID      string    `json:"agent_id"`
	FromRegionID string    `json:"from_region_id"`
	ToRegionID   string    `json:"to_region_id"`
	Reason       string    `json:"reason"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

type Cluster struct {
	ID         string    `json:"id"`
	RegionID   string    `json:"region_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	NodeCount  int       `json:"node_count"`
	AgentCount int       `json:"agent_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// ── Controller ────────────────────────────────────────────────────────────────

type RegionController struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *RegionController {
	return &RegionController{cfg: cfg, db: db, nc: nc, logger: slog.Default()}
}

// Register this region in the DB on startup
func (rc *RegionController) ensureRegionRegistered() {
	_, err := rc.db.Exec(`
		INSERT INTO deployment_regions (name, display_name, provider, status, is_primary, endpoint_url)
		VALUES ($1, $2, $3, 'active', $4, $5)
		ON CONFLICT (name) DO UPDATE SET status='active', provider=EXCLUDED.provider, updated_at=NOW()`,
		rc.cfg.RegionName,
		fmt.Sprintf("%s Region", rc.cfg.RegionName),
		rc.cfg.RegionProvider,
		rc.cfg.RegionName == "primary",
		fmt.Sprintf("http://region-controller-%s:%s", rc.cfg.RegionName, rc.cfg.Port),
	)
	if err != nil {
		rc.logger.Warn("could not register region", "error", err)
	} else {
		rc.logger.Info("region registered", "name", rc.cfg.RegionName)
	}
}

// Sync agent counts into region stats
func (rc *RegionController) syncRegionStats(ctx context.Context) {
	ticker := time.NewTicker(rc.cfg.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := rc.db.QueryContext(ctx,
				`SELECT r.name, COUNT(rp.id) FROM deployment_regions r LEFT JOIN region_placements rp ON rp.region_id=r.id AND rp.status='active' GROUP BY r.name`)
			if err != nil { continue }
			for rows.Next() {
				var name string; var count int
				rows.Scan(&name, &count)
				agentsInRegion.WithLabelValues(name).Set(float64(count))
			}
			rows.Close()

			// Detect unhealthy regions and trigger failover
			rc.detectAndFailover(ctx)
		}
	}
}

func (rc *RegionController) detectAndFailover(ctx context.Context) {
	threshold := time.Now().Add(-rc.cfg.FailoverThreshold)
	rows, err := rc.db.QueryContext(ctx,
		`SELECT id, name FROM deployment_regions WHERE status='degraded' AND updated_at < $1`, threshold)
	if err != nil { return }
	defer rows.Close()
	for rows.Next() {
		var regionID, regionName string
		rows.Scan(&regionID, &regionName)
		rc.triggerFailover(ctx, regionID, regionName)
	}
}

func (rc *RegionController) triggerFailover(ctx context.Context, fromRegionID, fromRegionName string) {
	// Find a healthy target region
	var toRegionID, toRegionName string
	rc.db.QueryRowContext(ctx,
		`SELECT id, name FROM deployment_regions WHERE status='active' AND id != $1 ORDER BY RANDOM() LIMIT 1`, fromRegionID).
		Scan(&toRegionID, &toRegionName)
	if toRegionID == "" { return }

	// Move placements
	result, err := rc.db.ExecContext(ctx,
		`UPDATE region_placements SET region_id=$1, updated_at=NOW() WHERE region_id=$2 AND status='active'`, toRegionID, fromRegionID)
	if err != nil { return }
	count, _ := result.RowsAffected()
	if count == 0 { return }

	// Record failover event
	rc.db.ExecContext(ctx,
		`INSERT INTO region_failover_events (from_region_id, to_region_id, reason, agents_migrated, status)
		 VALUES ($1, $2, 'region_degraded', $3, 'completed')`, fromRegionID, toRegionID, count)

	failoverEvents.WithLabelValues(fromRegionName, toRegionName).Add(float64(count))

	rc.nc.Publish("agents.events", mustJSON(map[string]interface{}{
		"event_type":   "region.failover_triggered",
		"from_region":  fromRegionName,
		"to_region":    toRegionName,
		"agents_moved": count,
		"source":       "region-controller",
	}))

	rc.logger.Info("failover completed", "from", fromRegionName, "to", toRegionName, "agents", count)
}

// ── HTTP Router ───────────────────────────────────────────────────────────────

func (rc *RegionController) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "region-controller", "region": rc.cfg.RegionName})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := rc.db.Ping(); err != nil { c.JSON(503, gin.H{"ready": false}); return }
		c.JSON(200, gin.H{"ready": true})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	api := r.Group("/api/v1")

	// ── Regions ──────────────────────────────────────────────────────────────
	api.GET("/regions", func(c *gin.Context) {
		rows, err := rc.db.QueryContext(c.Request.Context(),
			`SELECT r.id, r.name, r.display_name, r.provider, r.status, r.is_primary, r.endpoint_url,
			        COUNT(DISTINCT rp.id) AS agent_count,
			        COUNT(DISTINCT cl.id) AS cluster_count,
			        r.created_at, r.updated_at
			 FROM deployment_regions r
			 LEFT JOIN region_placements rp ON rp.region_id=r.id AND rp.status='active'
			 LEFT JOIN clusters cl ON cl.region_id=r.id AND cl.status='active'
			 GROUP BY r.id ORDER BY r.is_primary DESC, r.name`)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var regions []Region
		for rows.Next() {
			var reg Region
			rows.Scan(&reg.ID, &reg.Name, &reg.DisplayName, &reg.Provider, &reg.Status,
				&reg.IsPrimary, &reg.EndpointURL, &reg.AgentCount, &reg.ClusterCount, &reg.CreatedAt, &reg.UpdatedAt)
			regions = append(regions, reg)
		}
		if regions == nil { regions = []Region{} }
		c.JSON(200, regions)
	})

	api.POST("/regions", func(c *gin.Context) {
		var body struct {
			Name        string `json:"name" binding:"required"`
			DisplayName string `json:"display_name"`
			Provider    string `json:"provider"`
			IsPrimary   bool   `json:"is_primary"`
			EndpointURL string `json:"endpoint_url"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.DisplayName == "" { body.DisplayName = body.Name }
		if body.Provider == "" { body.Provider = "docker" }
		var id string
		err := rc.db.QueryRowContext(c.Request.Context(),
			`INSERT INTO deployment_regions (name, display_name, provider, status, is_primary, endpoint_url)
			 VALUES ($1, $2, $3, 'active', $4, $5) RETURNING id::text`,
			body.Name, body.DisplayName, body.Provider, body.IsPrimary, body.EndpointURL).Scan(&id)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(201, gin.H{"id": id, "name": body.Name, "status": "active"})
	})

	api.GET("/regions/:id", func(c *gin.Context) {
		var reg Region
		err := rc.db.QueryRowContext(c.Request.Context(),
			`SELECT r.id, r.name, r.display_name, r.provider, r.status, r.is_primary, r.endpoint_url,
			        COUNT(DISTINCT rp.id), COUNT(DISTINCT cl.id), r.created_at, r.updated_at
			 FROM deployment_regions r
			 LEFT JOIN region_placements rp ON rp.region_id=r.id AND rp.status='active'
			 LEFT JOIN clusters cl ON cl.region_id=r.id AND cl.status='active'
			 WHERE r.id=$1::uuid GROUP BY r.id`, c.Param("id")).
			Scan(&reg.ID, &reg.Name, &reg.DisplayName, &reg.Provider, &reg.Status, &reg.IsPrimary, &reg.EndpointURL,
				&reg.AgentCount, &reg.ClusterCount, &reg.CreatedAt, &reg.UpdatedAt)
		if err != nil { c.JSON(404, gin.H{"error": "region not found"}); return }
		c.JSON(200, reg)
	})

	api.PATCH("/regions/:id", func(c *gin.Context) {
		var body struct {
			Status      string `json:"status"`
			DisplayName string `json:"display_name"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		rc.db.ExecContext(c.Request.Context(),
			`UPDATE deployment_regions SET status=COALESCE(NULLIF($1,''), status), display_name=COALESCE(NULLIF($2,''), display_name), updated_at=NOW() WHERE id=$3::uuid`,
			body.Status, body.DisplayName, c.Param("id"))
		c.JSON(200, gin.H{"updated": true})
	})

	api.GET("/regions/:id/clusters", func(c *gin.Context) {
		rows, err := rc.db.QueryContext(c.Request.Context(),
			`SELECT id, region_id, name, status, node_count, agent_count, created_at FROM clusters WHERE region_id=$1::uuid ORDER BY created_at DESC`, c.Param("id"))
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var clusters []Cluster
		for rows.Next() {
			var cl Cluster
			rows.Scan(&cl.ID, &cl.RegionID, &cl.Name, &cl.Status, &cl.NodeCount, &cl.AgentCount, &cl.CreatedAt)
			clusters = append(clusters, cl)
		}
		if clusters == nil { clusters = []Cluster{} }
		c.JSON(200, clusters)
	})

	// ── Placements ───────────────────────────────────────────────────────────
	api.GET("/regions/placements", func(c *gin.Context) {
		limit := queryInt(c, "limit", 50)
		rows, err := rc.db.QueryContext(c.Request.Context(),
			`SELECT rp.id, rp.agent_id, rp.region_id, r.name AS region_name, rp.status, rp.placement_type, rp.created_at
			 FROM region_placements rp JOIN deployment_regions r ON r.id=rp.region_id
			 ORDER BY rp.created_at DESC LIMIT $1`, limit)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var placements []RegionPlacement
		for rows.Next() {
			var p RegionPlacement
			rows.Scan(&p.ID, &p.AgentID, &p.RegionID, &p.RegionName, &p.Status, &p.PlacementType, &p.CreatedAt)
			placements = append(placements, p)
		}
		if placements == nil { placements = []RegionPlacement{} }
		c.JSON(200, placements)
	})

	api.POST("/regions/placements", func(c *gin.Context) {
		var body struct {
			AgentID       string `json:"agent_id" binding:"required"`
			RegionID      string `json:"region_id" binding:"required"`
			PlacementType string `json:"placement_type"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.PlacementType == "" { body.PlacementType = "primary" }
		start := time.Now()
		var id string
		err := rc.db.QueryRowContext(c.Request.Context(),
			`INSERT INTO region_placements (agent_id, region_id, status, placement_type)
			 VALUES ($1::uuid, $2::uuid, 'active', $3) RETURNING id::text`,
			body.AgentID, body.RegionID, body.PlacementType).Scan(&id)
		placementLatency.Observe(time.Since(start).Seconds())
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		rc.nc.Publish("agents.events", mustJSON(map[string]interface{}{
			"event_type": "region.agent_placed",
			"agent_id":   body.AgentID,
			"region_id":  body.RegionID,
			"source":     "region-controller",
		}))
		c.JSON(201, gin.H{"id": id, "agent_id": body.AgentID, "region_id": body.RegionID})
	})

	// ── Failover Events ──────────────────────────────────────────────────────
	api.GET("/regions/failovers", func(c *gin.Context) {
		limit := queryInt(c, "limit", 20)
		rows, err := rc.db.QueryContext(c.Request.Context(),
			`SELECT fe.id, fe.from_region_id, fr.name, fe.to_region_id, tr.name, fe.reason, fe.status, fe.created_at
			 FROM region_failover_events fe
			 JOIN deployment_regions fr ON fr.id=fe.from_region_id
			 JOIN deployment_regions tr ON tr.id=fe.to_region_id
			 ORDER BY fe.created_at DESC LIMIT $1`, limit)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		type FailoverRow struct {
			ID           string    `json:"id"`
			FromRegionID string    `json:"from_region_id"`
			FromName     string    `json:"from_name"`
			ToRegionID   string    `json:"to_region_id"`
			ToName       string    `json:"to_name"`
			Reason       string    `json:"reason"`
			Status       string    `json:"status"`
			CreatedAt    time.Time `json:"created_at"`
		}
		var events []FailoverRow
		for rows.Next() {
			var e FailoverRow
			rows.Scan(&e.ID, &e.FromRegionID, &e.FromName, &e.ToRegionID, &e.ToName, &e.Reason, &e.Status, &e.CreatedAt)
			events = append(events, e)
		}
		if events == nil { events = []FailoverRow{} }
		c.JSON(200, events)
	})

	api.POST("/regions/failovers/trigger", func(c *gin.Context) {
		var body struct {
			FromRegionID string `json:"from_region_id" binding:"required"`
			Reason       string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.Reason == "" { body.Reason = "manual_trigger" }
		go rc.triggerFailover(context.Background(), body.FromRegionID, body.Reason)
		c.JSON(202, gin.H{"triggered": true, "from_region_id": body.FromRegionID})
	})

	// ── Clusters ─────────────────────────────────────────────────────────────
	api.GET("/clusters", func(c *gin.Context) {
		rows, err := rc.db.QueryContext(c.Request.Context(),
			`SELECT id, region_id, name, status, node_count, agent_count, created_at FROM clusters ORDER BY created_at DESC LIMIT 100`)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var clusters []Cluster
		for rows.Next() {
			var cl Cluster
			rows.Scan(&cl.ID, &cl.RegionID, &cl.Name, &cl.Status, &cl.NodeCount, &cl.AgentCount, &cl.CreatedAt)
			clusters = append(clusters, cl)
		}
		if clusters == nil { clusters = []Cluster{} }
		c.JSON(200, clusters)
	})

	api.POST("/clusters", func(c *gin.Context) {
		var body struct {
			RegionID  string `json:"region_id" binding:"required"`
			Name      string `json:"name" binding:"required"`
			NodeCount int    `json:"node_count"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.NodeCount <= 0 { body.NodeCount = 1 }
		var id string
		err := rc.db.QueryRowContext(c.Request.Context(),
			`INSERT INTO clusters (region_id, name, status, node_count) VALUES ($1::uuid, $2, 'active', $3) RETURNING id::text`,
			body.RegionID, body.Name, body.NodeCount).Scan(&id)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(201, gin.H{"id": id, "name": body.Name, "status": "active"})
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

	nc, err := natsgo.Connect(cfg.NatsURL, natsgo.RetryOnFailedConnect(true), natsgo.MaxReconnects(-1))
	if err != nil { logger.Warn("nats unavailable", "error", err) }
	defer func() { if nc != nil { nc.Close() } }()

	rc := New(cfg, db, nc)
	rc.ensureRegionRegistered()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.syncRegionStats(ctx)

	router := rc.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("region-controller listening", "port", cfg.Port, "region", cfg.RegionName)
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