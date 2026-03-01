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
	"sort"
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
	DatabaseURL   string
	NatsURL       string
	Port          string
	ScheduleEvery time.Duration
	HeartbeatEvery time.Duration
	FailoverThreshold time.Duration
}

func loadConfig() Config {
	sched, _ := time.ParseDuration(getEnv("SCHEDULE_INTERVAL", "10s"))
	hb, _ := time.ParseDuration(getEnv("HEARTBEAT_INTERVAL", "30s"))
	fo, _ := time.ParseDuration(getEnv("FAILOVER_THRESHOLD", "2m"))
	return Config{
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:           getEnv("NATS_URL", "nats://nats:4222"),
		Port:              getEnv("PORT", "8098"),
		ScheduleEvery:     sched,
		HeartbeatEvery:    hb,
		FailoverThreshold: fo,
	}
}

var (
	placementsScheduled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_global_placements_total",
	}, []string{"region"})
	failoversTriggered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_region_failovers_total",
	})
	regionsHealthy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_regions_healthy",
	})
)

func init() {
	prometheus.MustRegister(placementsScheduled, failoversTriggered, regionsHealthy)
}

type Region struct {
	ID            string
	Name          string
	Endpoint      string
	Status        string
	LastHeartbeat *time.Time
	LatencyMs     int
	TotalCPU      float64
	UsedCPU       float64
	TotalMemGB    float64
	UsedMemGB     float64
	RunningAgents int
}

type Placement struct {
	ID        string
	AgentID   string
	OrgID     string
	RegionID  string
	ClusterID string
	Type      string
	Status    string
	Priority  int
}

type GlobalScheduler struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
	mu     sync.RWMutex
	regions map[string]*Region
}

func NewGlobalScheduler(cfg Config, db *sql.DB, nc *natsgo.Conn) *GlobalScheduler {
	return &GlobalScheduler{
		cfg:     cfg,
		db:      db,
		nc:      nc,
		http:    &http.Client{Timeout: 10 * time.Second},
		logger:  slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		regions: make(map[string]*Region),
	}
}

// ── Region discovery & health ─────────────────────────────────────────────────

func (s *GlobalScheduler) RefreshRegions(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, name, COALESCE(endpoint,''), status,
		       last_heartbeat, COALESCE(latency_ms,0)
		FROM regions WHERE status != 'offline'`)
	if err != nil {
		s.logger.Error("refresh regions failed", "error", err)
		return
	}
	defer rows.Close()

	s.mu.Lock()
	defer s.mu.Unlock()

	healthy := 0
	for rows.Next() {
		var r Region
		var lastHB *time.Time
		rows.Scan(&r.ID, &r.Name, &r.Endpoint, &r.Status, &lastHB, &r.LatencyMs)
		r.LastHeartbeat = lastHB
		s.regions[r.ID] = &r

		// Check for dead regions
		if lastHB != nil && time.Since(*lastHB) > s.cfg.FailoverThreshold {
			if r.Status == "active" {
				go s.triggerFailover(ctx, r.ID)
			}
		} else if r.Status == "active" {
			healthy++
		}
	}
	regionsHealthy.Set(float64(healthy))
}

func (s *GlobalScheduler) ProbeRegions(ctx context.Context) {
	s.mu.RLock()
	regions := make([]*Region, 0, len(s.regions))
	for _, r := range s.regions {
		if r.Endpoint != "" {
			regions = append(regions, r)
		}
	}
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, r := range regions {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			req, _ := http.NewRequestWithContext(ctx, "GET", r.Endpoint+"/health", nil)
			resp, err := s.http.Do(req)
			latency := int(time.Since(start).Milliseconds())

			if err != nil || resp.StatusCode >= 500 {
				s.db.ExecContext(ctx, `UPDATE regions SET status='degraded', latency_ms=$1 WHERE id=$2::uuid`,
					latency, r.ID)
				return
			}
			resp.Body.Close()

			s.db.ExecContext(ctx, `
				UPDATE regions SET last_heartbeat=NOW(), latency_ms=$1, status='active'
				WHERE id=$2::uuid`, latency, r.ID)
		}()
	}
	wg.Wait()
}

// ── Placement logic ───────────────────────────────────────────────────────────

func (s *GlobalScheduler) Schedule(ctx context.Context) {
	// Find pending placements
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, agent_id::text, COALESCE(organisation_id::text,''),
		       placement_type, priority
		FROM global_placements WHERE status='pending'
		ORDER BY priority ASC, created_at ASC
		LIMIT 50`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var p Placement
		rows.Scan(&p.ID, &p.AgentID, &p.OrgID, &p.Type, &p.Priority)

		region, err := s.selectRegion(ctx, p)
		if err != nil {
			s.logger.Warn("no suitable region", "agent_id", p.AgentID, "error", err)
			continue
		}

		cluster, err := s.selectCluster(ctx, region.ID)
		if err != nil {
			s.logger.Warn("no cluster in region", "region", region.Name, "error", err)
			continue
		}

		_, err = s.db.ExecContext(ctx, `
			UPDATE global_placements
			SET region_id=$1::uuid, cluster_id=$2::uuid, status='scheduled', scheduled_at=NOW()
			WHERE id=$3::uuid`,
			region.ID, cluster, p.ID)
		if err != nil {
			continue
		}

		placementsScheduled.With(prometheus.Labels{"region": region.Name}).Inc()

		// Dispatch to region controller
		s.dispatchToRegion(ctx, region, p)
		s.logger.Info("agent placed", "agent_id", p.AgentID, "region", region.Name, "cluster", cluster)
	}
}

func (s *GlobalScheduler) selectRegion(ctx context.Context, p Placement) (*Region, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Score regions by available capacity and latency
	type scored struct {
		region *Region
		score  float64
	}
	var candidates []scored

	for _, r := range s.regions {
		if r.Status != "active" {
			continue
		}
		// Score = available capacity weight (70%) + latency weight (30%)
		cpuAvail := 1.0
		if r.TotalCPU > 0 {
			cpuAvail = 1.0 - (r.UsedCPU / r.TotalCPU)
		}
		latencyScore := 1.0
		if r.LatencyMs > 0 {
			latencyScore = 1.0 / float64(r.LatencyMs)
		}
		score := cpuAvail*0.7 + latencyScore*0.3
		candidates = append(candidates, scored{region: r, score: score})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no active regions available")
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	return candidates[0].region, nil
}

func (s *GlobalScheduler) selectCluster(ctx context.Context, regionID string) (string, error) {
	var clusterID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text FROM clusters
		WHERE region_id=$1::uuid AND status='active'
		ORDER BY (1.0 - used_cpu_cores/NULLIF(total_cpu_cores,0)) DESC
		LIMIT 1`, regionID).Scan(&clusterID)
	if err != nil {
		// Create a default cluster record for this region
		s.db.QueryRowContext(ctx, `
			INSERT INTO clusters (region_id, name, status)
			VALUES ($1::uuid, 'default', 'active')
			ON CONFLICT DO NOTHING
			RETURNING id::text`, regionID).Scan(&clusterID)
	}
	return clusterID, nil
}

func (s *GlobalScheduler) dispatchToRegion(ctx context.Context, region *Region, p Placement) {
	if region.Endpoint == "" {
		// Local region — dispatch via NATS
		payload, _ := json.Marshal(map[string]interface{}{
			"placement_id": p.ID,
			"agent_id":     p.AgentID,
			"org_id":       p.OrgID,
			"region":       region.Name,
			"action":       "schedule",
		})
		s.nc.Publish("global.placement.dispatch", payload)
		return
	}

	// Remote region — HTTP dispatch
	payload, _ := json.Marshal(map[string]interface{}{
		"placement_id": p.ID,
		"agent_id":     p.AgentID,
		"org_id":       p.OrgID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		region.Endpoint+"/api/v1/agents/"+p.AgentID+"/schedule",
		bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// ── Failover ──────────────────────────────────────────────────────────────────

func (s *GlobalScheduler) triggerFailover(ctx context.Context, deadRegionID string) {
	// Mark region as degraded
	s.db.ExecContext(ctx, `UPDATE regions SET status='degraded' WHERE id=$1::uuid`, deadRegionID)

	// Find agents placed in the dead region
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, agent_id::text, COALESCE(organisation_id::text,'')
		FROM global_placements
		WHERE region_id=$1::uuid AND status='running'`, deadRegionID)
	if err != nil {
		return
	}
	defer rows.Close()

	var placements []Placement
	for rows.Next() {
		var p Placement
		rows.Scan(&p.ID, &p.AgentID, &p.OrgID)
		placements = append(placements, p)
	}

	if len(placements) == 0 {
		return
	}

	// Find target region for migration
	targetRegion, err := s.selectRegion(ctx, Placement{})
	if err != nil {
		s.logger.Error("no failover region available", "source_region", deadRegionID)
		return
	}

	// Mark current placements as migrating
	s.db.ExecContext(ctx, `
		UPDATE global_placements SET status='migrating'
		WHERE region_id=$1::uuid AND status='running'`, deadRegionID)

	// Create failover event
	var failoverID string
	s.db.QueryRowContext(ctx, `
		INSERT INTO region_failover_events
		  (source_region_id, target_region_id, trigger_reason, agents_migrated, status)
		VALUES ($1::uuid, $2::uuid, 'heartbeat_timeout', $3, 'in_progress')
		RETURNING id::text`,
		deadRegionID, targetRegion.ID, len(placements)).Scan(&failoverID)

	// Re-queue all placements to target region
	for _, p := range placements {
		s.db.ExecContext(ctx, `
			INSERT INTO global_placements
			  (agent_id, organisation_id, region_id, placement_type, status, migrated_from)
			SELECT agent_id, organisation_id, $1::uuid, placement_type, 'pending', id
			FROM global_placements WHERE id=$2::uuid`,
			targetRegion.ID, p.ID)
	}

	// Update failover event
	s.db.ExecContext(ctx, `UPDATE region_failover_events SET status='completed', completed_at=NOW() WHERE id=$1::uuid`, failoverID)

	failoversTriggered.Inc()
	s.logger.Warn("region failover triggered",
		"source_region", deadRegionID,
		"target_region", targetRegion.Name,
		"agents_migrated", len(placements))

	payload, _ := json.Marshal(map[string]interface{}{
		"source_region_id": deadRegionID,
		"target_region_id": targetRegion.ID,
		"agents_migrated":  len(placements),
	})
	s.nc.Publish("region.failover.completed", payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (s *GlobalScheduler) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "global-scheduler"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := s.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// List regions with health status
	r.GET("/regions", func(c *gin.Context) {
		rows, err := s.db.QueryContext(c.Request.Context(), `
			SELECT r.id::text, r.name, r.display_name, r.provider, r.status,
			       r.last_heartbeat, r.latency_ms, r.supports_k8s, r.supports_gpu,
			       COUNT(c.id) as cluster_count,
			       COALESCE(SUM(c.running_agents),0) as running_agents
			FROM regions r
			LEFT JOIN clusters c ON c.region_id=r.id AND c.status='active'
			GROUP BY r.id ORDER BY r.name`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var regions []map[string]interface{}
		for rows.Next() {
			var id, name, displayName, provider, status string
			var lastHB *time.Time
			var latency, clusters, running int
			var k8s, gpu bool
			rows.Scan(&id, &name, &displayName, &provider, &status, &lastHB,
				&latency, &k8s, &gpu, &clusters, &running)
			regions = append(regions, map[string]interface{}{
				"id": id, "name": name, "display_name": displayName,
				"provider": provider, "status": status,
				"last_heartbeat": lastHB, "latency_ms": latency,
				"supports_k8s": k8s, "supports_gpu": gpu,
				"cluster_count": clusters, "running_agents": running,
			})
		}
		if regions == nil {
			regions = []map[string]interface{}{}
		}
		c.JSON(200, regions)
	})

	// Register a region
	r.POST("/regions", func(c *gin.Context) {
		var body struct {
			Name        string  `json:"name" binding:"required"`
			DisplayName string  `json:"display_name"`
			Provider    string  `json:"provider"`
			Endpoint    string  `json:"endpoint"`
			Latitude    float64 `json:"latitude"`
			Longitude   float64 `json:"longitude"`
			Country     string  `json:"country"`
			SupportsK8s bool    `json:"supports_k8s"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.Provider == "" {
			body.Provider = "self-hosted"
		}
		var id string
		err := s.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO regions (name, display_name, provider, endpoint, latitude, longitude, country, supports_k8s)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (name) DO UPDATE SET endpoint=EXCLUDED.endpoint, updated_at=NOW()
			RETURNING id::text`,
			body.Name, body.DisplayName, body.Provider, body.Endpoint,
			body.Latitude, body.Longitude, body.Country, body.SupportsK8s).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id, "name": body.Name})
	})

	// Schedule agent globally
	r.POST("/placements", func(c *gin.Context) {
		var body struct {
			AgentID   string `json:"agent_id" binding:"required"`
			OrgID     string `json:"organisation_id"`
			RegionPref string `json:"preferred_region"`
			Type      string `json:"placement_type"`
			Priority  int    `json:"priority"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.Type == "" {
			body.Type = "primary"
		}
		if body.Priority == 0 {
			body.Priority = 5
		}
		constraints := map[string]interface{}{}
		if body.RegionPref != "" {
			constraints["preferred_region"] = body.RegionPref
		}
		constraintsJSON, _ := json.Marshal(constraints)
		var id string
		err := s.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO global_placements
			  (agent_id, organisation_id, region_id, placement_type, priority, status, constraints)
			SELECT $1::uuid, NULLIF($2,'')::uuid,
			       COALESCE((SELECT id FROM regions WHERE name=$3 LIMIT 1),
			                (SELECT id FROM regions WHERE status='active' ORDER BY latency_ms ASC LIMIT 1)),
			       $4, $5, 'pending', $6::jsonb
			RETURNING id::text`,
			body.AgentID, body.OrgID, body.RegionPref, body.Type, body.Priority,
			string(constraintsJSON)).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(202, gin.H{"placement_id": id, "status": "pending"})
	})

	// Global agent status across regions
	r.GET("/agents/:agent_id/placements", func(c *gin.Context) {
		rows, err := s.db.QueryContext(c.Request.Context(), `
			SELECT gp.id::text, r.name, r.status as region_status,
			       gp.placement_type, gp.status, gp.scheduled_at
			FROM global_placements gp
			JOIN regions r ON r.id=gp.region_id
			WHERE gp.agent_id=$1::uuid
			ORDER BY gp.created_at DESC`, c.Param("agent_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var placements []map[string]interface{}
		for rows.Next() {
			var id, region, regionStatus, pType, status string
			var scheduledAt *time.Time
			rows.Scan(&id, &region, &regionStatus, &pType, &status, &scheduledAt)
			placements = append(placements, map[string]interface{}{
				"id": id, "region": region, "region_status": regionStatus,
				"placement_type": pType, "status": status, "scheduled_at": scheduledAt,
			})
		}
		if placements == nil {
			placements = []map[string]interface{}{}
		}
		c.JSON(200, placements)
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting global-scheduler", "port", cfg.Port)

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
			natsgo.Name("global-scheduler"))
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

	sched := NewGlobalScheduler(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initial region refresh
	sched.RefreshRegions(ctx)

	// Background loops
	go func() {
		schedTicker := time.NewTicker(cfg.ScheduleEvery)
		probeTicker := time.NewTicker(cfg.HeartbeatEvery)
		defer schedTicker.Stop()
		defer probeTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-schedTicker.C:
				sched.RefreshRegions(ctx)
				sched.Schedule(ctx)
			case <-probeTicker.C:
				sched.ProbeRegions(ctx)
			}
		}
	}()

	router := sched.setupRouter()
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
	logger.Info("global-scheduler stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}