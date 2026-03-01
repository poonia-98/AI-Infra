// AgentPlane Control Brain — Central Orchestration Service
// The "Kubernetes control plane" for AI agents.
// Responsible for: global state, service registry, placement decisions,
// autoscale decisions, health reconciliation, failover orchestration.
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

// ═══════════════════════════════════════════════════════════════════
// CONFIG
// ═══════════════════════════════════════════════════════════════════

type Config struct {
	Port              string
	DatabaseURL       string
	NatsURL           string
	ExecutorURL       string
	AutoscalerURL     string
	SchedulerURL      string
	NodeManagerURL    string
	MemoryURL         string
	FederationURL     string
	RegionCtrlURL     string
	ClusterCtrlURL    string
	HeartbeatInterval time.Duration
	ReconcileInterval time.Duration
	ServiceTTL        time.Duration
}

func loadConfig() Config {
	hi, _ := time.ParseDuration(env("HEARTBEAT_INTERVAL", "15s"))
	ri, _ := time.ParseDuration(env("RECONCILE_INTERVAL", "10s"))
	return Config{
		Port:              env("PORT", "8200"),
		DatabaseURL:       env("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:           env("NATS_URL", "nats://nats:4222"),
		ExecutorURL:       env("EXECUTOR_URL", "http://executor:8081"),
		AutoscalerURL:     env("AUTOSCALER_URL", "http://agent-autoscaler:8093"),
		SchedulerURL:      env("SCHEDULER_URL", "http://global-scheduler:8098"),
		NodeManagerURL:    env("NODE_MANAGER_URL", "http://node-manager:8087"),
		MemoryURL:         env("MEMORY_URL", "http://memory-vector-engine:8103"),
		FederationURL:     env("FEDERATION_URL", "http://agent-federation-network:8107"),
		RegionCtrlURL:     env("REGION_CTRL_URL", "http://region-controller:8099"),
		ClusterCtrlURL:    env("CLUSTER_CTRL_URL", "http://cluster-controller:8108"),
		HeartbeatInterval: hi,
		ReconcileInterval: ri,
		ServiceTTL:        60 * time.Second,
	}
}

// ═══════════════════════════════════════════════════════════════════
// PROMETHEUS
// ═══════════════════════════════════════════════════════════════════

var (
	eventsProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "brain_events_processed_total"},
		[]string{"event_type"},
	)
	agentsTracked = prometheus.NewGauge(prometheus.GaugeOpts{Name: "brain_agents_tracked"})
	reconcileRuns = prometheus.NewCounter(prometheus.CounterOpts{Name: "brain_reconcile_runs_total"})
	serviceHealthG = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "brain_service_health"},
		[]string{"service"},
	)
	decisionsMade = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "brain_decisions_total"},
		[]string{"decision_type"},
	)
	reconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "brain_reconcile_duration_seconds",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	prometheus.MustRegister(eventsProcessed, agentsTracked, reconcileRuns,
		serviceHealthG, decisionsMade, reconcileDuration)
}

// ═══════════════════════════════════════════════════════════════════
// STATE TYPES
// ═══════════════════════════════════════════════════════════════════

type AgentState struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Status         string    `json:"status"`
	OrganisationID string    `json:"organisation_id"`
	Region         string    `json:"region"`
	NodeID         string    `json:"node_id"`
	CPUUsage       float64   `json:"cpu_usage"`
	MemoryUsageMB  float64   `json:"memory_usage_mb"`
	RestartCount   int       `json:"restart_count"`
	ErrorCount     int       `json:"error_count"`
	LastSeen       time.Time `json:"last_seen"`
	LastEvent      string    `json:"last_event"`
}

type ServiceRecord struct {
	ServiceName    string    `json:"service_name"`
	ServiceURL     string    `json:"service_url"`
	HealthEndpoint string    `json:"health_endpoint"`
	Capabilities   []string  `json:"capabilities"`
	Status         string    `json:"status"` // healthy | degraded | down
	Node           string    `json:"node"`
	Version        string    `json:"version"`
	LastSeen       time.Time `json:"last_seen"`
	RegisteredAt   time.Time `json:"registered_at"`
}

type NodeRecord struct {
	NodeID        string    `json:"node_id"`
	Hostname      string    `json:"hostname"`
	Region        string    `json:"region"`
	CPUCores      int       `json:"cpu_cores"`
	MemoryGB      float64   `json:"memory_gb"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	AgentCount    int       `json:"agent_count"`
	Status        string    `json:"status"`
	LastSeen      time.Time `json:"last_seen"`
}

type Decision struct {
	ID           string                 `json:"id"`
	DecisionType string                 `json:"decision_type"` // scale_up|scale_down|migrate|failover|restart
	AgentID      string                 `json:"agent_id,omitempty"`
	ServiceName  string                 `json:"service_name,omitempty"`
	Reason       string                 `json:"reason"`
	Action       map[string]interface{} `json:"action"`
	Status       string                 `json:"status"` // pending|executed|failed
	CreatedAt    time.Time              `json:"created_at"`
}

type PlatformState struct {
	mu             sync.RWMutex
	Agents         map[string]*AgentState  `json:"agents"`
	Services       map[string]*ServiceRecord `json:"services"`
	Nodes          map[string]*NodeRecord  `json:"nodes"`
	Decisions      []*Decision             `json:"decisions"`
	TotalEvents    int64                   `json:"total_events"`
	LastReconcile  time.Time               `json:"last_reconcile"`
	StartedAt      time.Time               `json:"started_at"`
}

func newPlatformState() *PlatformState {
	return &PlatformState{
		Agents:    make(map[string]*AgentState),
		Services:  make(map[string]*ServiceRecord),
		Nodes:     make(map[string]*NodeRecord),
		Decisions: make([]*Decision, 0),
		StartedAt: time.Now().UTC(),
	}
}

func (ps *PlatformState) statusSummary() map[string]interface{} {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	counts := map[string]int{"running": 0, "stopped": 0, "error": 0, "idle": 0}
	for _, a := range ps.Agents {
		counts[a.Status]++
	}
	healthySvcs, totalSvcs := 0, len(ps.Services)
	for _, s := range ps.Services {
		if s.Status == "healthy" { healthySvcs++ }
	}
	return map[string]interface{}{
		"agents_total":    len(ps.Agents),
		"agents_running":  counts["running"],
		"agents_error":    counts["error"],
		"services_total":  totalSvcs,
		"services_healthy": healthySvcs,
		"nodes_total":     len(ps.Nodes),
		"total_events":    ps.TotalEvents,
		"last_reconcile":  ps.LastReconcile,
		"uptime_seconds":  time.Since(ps.StartedAt).Seconds(),
	}
}

// ═══════════════════════════════════════════════════════════════════
// CONTROL BRAIN
// ═══════════════════════════════════════════════════════════════════

type Brain struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	state  *PlatformState
	logger *slog.Logger
	client *http.Client
}

func NewBrain(cfg Config, db *sql.DB, nc *natsgo.Conn) *Brain {
	return &Brain{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		state:  newPlatformState(),
		logger: slog.Default(),
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// ── Warm state from database ──────────────────────────────────────

func (b *Brain) warmState(ctx context.Context) {
	// Load agents
	rows, err := b.db.QueryContext(ctx,
		`SELECT id::text, name, status, COALESCE(organisation_id::text,''), restart_count FROM agents WHERE status NOT IN ('deleted') LIMIT 5000`)
	if err == nil {
		defer rows.Close()
		b.state.mu.Lock()
		for rows.Next() {
			a := &AgentState{LastSeen: time.Now()}
			rows.Scan(&a.ID, &a.Name, &a.Status, &a.OrganisationID, &a.RestartCount)
			b.state.Agents[a.ID] = a
		}
		b.state.mu.Unlock()
		agentsTracked.Set(float64(len(b.state.Agents)))
	}

	// Load service registry
	srows, err := b.db.QueryContext(ctx,
		`SELECT service_name, service_url, health_endpoint, capabilities, status, COALESCE(node,''), COALESCE(version,''), created_at FROM service_registry`)
	if err == nil {
		defer srows.Close()
		b.state.mu.Lock()
		for srows.Next() {
			s := &ServiceRecord{LastSeen: time.Now()}
			var caps []byte
			srows.Scan(&s.ServiceName, &s.ServiceURL, &s.HealthEndpoint, &caps, &s.Status, &s.Node, &s.Version, &s.RegisteredAt)
			if caps != nil { json.Unmarshal(caps, &s.Capabilities) }
			b.state.Services[s.ServiceName] = s
		}
		b.state.mu.Unlock()
	}

	b.logger.Info("state warmed", "agents", len(b.state.Agents), "services", len(b.state.Services))
}

// ── NATS event handler ─────────────────────────────────────────────

func (b *Brain) handleNATSEvent(msg *natsgo.Msg) {
	var evt map[string]interface{}
	if err := json.Unmarshal(msg.Data, &evt); err != nil { return }

	evtType, _ := evt["event_type"].(string)
	eventsProcessed.WithLabelValues(evtType).Inc()

	b.state.mu.Lock()
	b.state.TotalEvents++
	b.state.mu.Unlock()

	switch evtType {
	case "AgentStarted", "agent.started":
		if id, ok := evt["agent_id"].(string); ok {
			b.state.mu.Lock()
			if a, exists := b.state.Agents[id]; exists {
				a.Status = "running"
				a.LastSeen = time.Now()
				a.LastEvent = evtType
			} else {
				b.state.Agents[id] = &AgentState{ID: id, Status: "running", LastSeen: time.Now(), LastEvent: evtType}
			}
			agentsTracked.Set(float64(len(b.state.Agents)))
			b.state.mu.Unlock()
		}

	case "AgentStopped", "agent.stopped":
		if id, ok := evt["agent_id"].(string); ok {
			b.state.mu.Lock()
			if a, exists := b.state.Agents[id]; exists {
				a.Status = "stopped"
				a.LastSeen = time.Now()
			}
			b.state.mu.Unlock()
		}

	case "AgentFailed", "agent.error":
		if id, ok := evt["agent_id"].(string); ok {
			b.state.mu.Lock()
			if a, exists := b.state.Agents[id]; exists {
				a.Status = "error"
				a.ErrorCount++
				a.LastSeen = time.Now()
				// Auto-restart decision if too many errors
				if a.ErrorCount >= 3 && a.RestartCount < 5 {
					go b.issueDecision("restart", a.ID, "", "auto_restart_on_error", map[string]interface{}{
						"agent_id": a.ID, "action": "restart",
					})
				}
			}
			b.state.mu.Unlock()
		}

	case "metrics.update":
		if id, ok := evt["agent_id"].(string); ok {
			b.state.mu.Lock()
			if a, exists := b.state.Agents[id]; exists {
				if cpu, ok := evt["cpu_percent"].(float64); ok { a.CPUUsage = cpu }
				if mem, ok := evt["memory_mb"].(float64); ok { a.MemoryUsageMB = mem }
				a.LastSeen = time.Now()
			}
			b.state.mu.Unlock()
		}

	case "node.heartbeat":
		if nodeID, ok := evt["node_id"].(string); ok {
			b.state.mu.Lock()
			n := &NodeRecord{NodeID: nodeID, LastSeen: time.Now(), Status: "healthy"}
			if h, ok := evt["hostname"].(string); ok { n.Hostname = h }
			if r, ok := evt["region"].(string); ok { n.Region = r }
			if c, ok := evt["cpu_percent"].(float64); ok { n.CPUPercent = c }
			if m, ok := evt["memory_percent"].(float64); ok { n.MemoryPercent = m }
			if a, ok := evt["agent_count"].(float64); ok { n.AgentCount = int(a) }
			b.state.Nodes[nodeID] = n
			b.state.mu.Unlock()
		}

	case "service.registered":
		if name, ok := evt["service_name"].(string); ok {
			b.state.mu.Lock()
			svc := &ServiceRecord{ServiceName: name, LastSeen: time.Now(), Status: "healthy", RegisteredAt: time.Now()}
			if url, ok := evt["service_url"].(string); ok { svc.ServiceURL = url }
			if he, ok := evt["health_endpoint"].(string); ok { svc.HealthEndpoint = he }
			b.state.Services[name] = svc
			serviceHealthG.WithLabelValues(name).Set(1)
			b.state.mu.Unlock()
		}
	}
}

// ── Service health check loop ──────────────────────────────────────

func (b *Brain) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(b.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C: b.checkAllServices(ctx)
		}
	}
}

func (b *Brain) checkAllServices(ctx context.Context) {
	b.state.mu.RLock()
	services := make(map[string]*ServiceRecord, len(b.state.Services))
	for k, v := range b.state.Services { services[k] = v }
	b.state.mu.RUnlock()

	for name, svc := range services {
		endpoint := svc.HealthEndpoint
		if endpoint == "" { endpoint = svc.ServiceURL + "/health" }

		reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := b.client.Get(endpoint)
		cancel()

		status := "healthy"
		if err != nil || resp == nil || resp.StatusCode >= 400 {
			status = "down"
			serviceHealthG.WithLabelValues(name).Set(0)
			// persist to DB
			b.db.ExecContext(ctx,
				`UPDATE service_registry SET status='down', last_seen=NOW() WHERE service_name=$1`, name)
		} else {
			resp.Body.Close()
			serviceHealthG.WithLabelValues(name).Set(1)
			b.db.ExecContext(ctx,
				`UPDATE service_registry SET status='healthy', last_seen=NOW() WHERE service_name=$1`, name)
		}

		b.state.mu.Lock()
		if s, ok := b.state.Services[name]; ok {
			s.Status = status
			s.LastSeen = time.Now()
		}
		b.state.mu.Unlock()
	}
}

// ── Reconciliation loop ───────────────────────────────────────────

func (b *Brain) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(b.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C: b.reconcile(ctx)
		}
	}
}

func (b *Brain) reconcile(ctx context.Context) {
	start := time.Now()
	reconcileRuns.Inc()

	// Fetch actual agent states from DB
	rows, err := b.db.QueryContext(ctx,
		`SELECT id::text, name, status, restart_count FROM agents WHERE status NOT IN ('deleted')`)
	if err != nil {
		b.logger.Error("reconcile db fetch failed", "error", err)
		return
	}
	defer rows.Close()

	dbAgents := make(map[string]string)
	for rows.Next() {
		var id, name, status string; var rc int
		rows.Scan(&id, &name, &status, &rc)
		dbAgents[id] = status
	}

	// Find divergence: stuck-running agents not in DB
	b.state.mu.Lock()
	for id, a := range b.state.Agents {
		dbStatus, inDB := dbAgents[id]
		if !inDB {
			delete(b.state.Agents, id)
			continue
		}
		if a.Status != dbStatus {
			a.Status = dbStatus
		}
		// Detect zombie: status=running but not seen in >5 minutes
		if a.Status == "running" && time.Since(a.LastSeen) > 5*time.Minute {
			a.Status = "zombie"
			go b.issueDecision("restart", a.ID, "", "zombie_detected", map[string]interface{}{
				"agent_id": a.ID,
			})
		}
	}
	// Add new agents from DB not yet tracked
	for id, status := range dbAgents {
		if _, ok := b.state.Agents[id]; !ok {
			b.state.Agents[id] = &AgentState{ID: id, Status: status, LastSeen: time.Now()}
		}
	}
	b.state.LastReconcile = time.Now()
	agentsTracked.Set(float64(len(b.state.Agents)))
	b.state.mu.Unlock()

	reconcileDuration.Observe(time.Since(start).Seconds())
}

// ── Decision engine ───────────────────────────────────────────────

func (b *Brain) issueDecision(dtype, agentID, serviceName, reason string, action map[string]interface{}) {
	d := &Decision{
		ID:           fmt.Sprintf("%d", time.Now().UnixNano()),
		DecisionType: dtype,
		AgentID:      agentID,
		ServiceName:  serviceName,
		Reason:       reason,
		Action:       action,
		Status:       "pending",
		CreatedAt:    time.Now(),
	}

	b.state.mu.Lock()
	b.state.Decisions = append(b.state.Decisions, d)
	// Keep last 500 decisions
	if len(b.state.Decisions) > 500 { b.state.Decisions = b.state.Decisions[len(b.state.Decisions)-500:] }
	b.state.mu.Unlock()

	decisionsMade.WithLabelValues(dtype).Inc()

	// Persist to DB
	actionJSON, _ := json.Marshal(action)
	b.db.Exec(`INSERT INTO brain_decisions (decision_type, agent_id, service_name, reason, action, status) VALUES ($1, NULLIF($2,'')::uuid, NULLIF($3,''), $4, $5::jsonb, 'pending') ON CONFLICT DO NOTHING`,
		dtype, agentID, serviceName, reason, string(actionJSON))

	// Publish to NATS
	if b.nc != nil {
		b.nc.Publish("brain.commands", mustJSON(map[string]interface{}{
			"decision_id":   d.ID,
			"decision_type": dtype,
			"agent_id":      agentID,
			"reason":        reason,
			"action":        action,
			"ts":            time.Now().UTC(),
		}))
	}
}

// ── HTTP Router ───────────────────────────────────────────────────

func (b *Brain) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// ── Health / Metrics ────────────────────────────────────────────
	r.GET("/health", func(c *gin.Context) {
		dbOk := b.db.Ping() == nil
		c.JSON(200, gin.H{"status": "healthy", "db": dbOk, "service": "control-brain"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := b.db.Ping(); err != nil { c.JSON(503, gin.H{"ready": false}); return }
		c.JSON(200, gin.H{"ready": true})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	api := r.Group("/api/v1/brain")

	// ── Platform state ───────────────────────────────────────────────
	api.GET("/state", func(c *gin.Context) {
		b.state.mu.RLock()
		defer b.state.mu.RUnlock()
		c.JSON(200, gin.H{
			"summary":  b.state.statusSummary(),
			"services": b.state.Services,
			"nodes":    b.state.Nodes,
		})
	})

	api.GET("/state/agents", func(c *gin.Context) {
		b.state.mu.RLock()
		agents := make([]*AgentState, 0, len(b.state.Agents))
		for _, a := range b.state.Agents { agents = append(agents, a) }
		b.state.mu.RUnlock()
		c.JSON(200, gin.H{"agents": agents, "total": len(agents)})
	})

	api.GET("/state/agents/:id", func(c *gin.Context) {
		b.state.mu.RLock()
		a, ok := b.state.Agents[c.Param("id")]
		b.state.mu.RUnlock()
		if !ok { c.JSON(404, gin.H{"error": "agent not in brain state"}); return }
		c.JSON(200, a)
	})

	api.GET("/state/summary", func(c *gin.Context) {
		c.JSON(200, b.state.statusSummary())
	})

	// ── Service registry ────────────────────────────────────────────
	api.GET("/services", func(c *gin.Context) {
		rows, err := b.db.QueryContext(c.Request.Context(),
			`SELECT service_name, service_url, health_endpoint, capabilities, status, COALESCE(node,''), COALESCE(version,''), last_seen, created_at FROM service_registry ORDER BY service_name`)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var svcs []ServiceRecord
		for rows.Next() {
			var s ServiceRecord; var caps []byte
			rows.Scan(&s.ServiceName, &s.ServiceURL, &s.HealthEndpoint, &caps, &s.Status, &s.Node, &s.Version, &s.LastSeen, &s.RegisteredAt)
			if caps != nil { json.Unmarshal(caps, &s.Capabilities) }
			svcs = append(svcs, s)
		}
		if svcs == nil { svcs = []ServiceRecord{} }
		c.JSON(200, svcs)
	})

	api.POST("/services/register", func(c *gin.Context) {
		var body struct {
			ServiceName    string   `json:"service_name" binding:"required"`
			ServiceURL     string   `json:"service_url" binding:"required"`
			HealthEndpoint string   `json:"health_endpoint"`
			Capabilities   []string `json:"capabilities"`
			Node           string   `json:"node"`
			Version        string   `json:"version"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.HealthEndpoint == "" { body.HealthEndpoint = body.ServiceURL + "/health" }
		caps, _ := json.Marshal(body.Capabilities)

		_, err := b.db.ExecContext(c.Request.Context(), `
			INSERT INTO service_registry (service_name, service_url, health_endpoint, capabilities, status, node, version, last_seen)
			VALUES ($1, $2, $3, $4::jsonb, 'healthy', $5, $6, NOW())
			ON CONFLICT (service_name) DO UPDATE SET
				service_url=EXCLUDED.service_url, health_endpoint=EXCLUDED.health_endpoint,
				capabilities=EXCLUDED.capabilities, status='healthy', node=EXCLUDED.node,
				version=EXCLUDED.version, last_seen=NOW()`,
			body.ServiceName, body.ServiceURL, body.HealthEndpoint,
			string(caps), body.Node, body.Version)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }

		// Update in-memory
		b.state.mu.Lock()
		b.state.Services[body.ServiceName] = &ServiceRecord{
			ServiceName: body.ServiceName, ServiceURL: body.ServiceURL,
			HealthEndpoint: body.HealthEndpoint, Capabilities: body.Capabilities,
			Status: "healthy", Node: body.Node, Version: body.Version,
			LastSeen: time.Now(), RegisteredAt: time.Now(),
		}
		serviceHealthG.WithLabelValues(body.ServiceName).Set(1)
		b.state.mu.Unlock()

		// Broadcast on NATS
		if b.nc != nil {
			b.nc.Publish("agents.events", mustJSON(map[string]interface{}{
				"event_type": "service.registered",
				"service_name": body.ServiceName,
				"service_url":  body.ServiceURL,
			}))
		}

		b.logger.Info("service registered", "name", body.ServiceName)
		c.JSON(200, gin.H{"registered": true, "service": body.ServiceName})
	})

	api.DELETE("/services/:name", func(c *gin.Context) {
		b.db.ExecContext(c.Request.Context(), `DELETE FROM service_registry WHERE service_name=$1`, c.Param("name"))
		b.state.mu.Lock()
		delete(b.state.Services, c.Param("name"))
		b.state.mu.Unlock()
		c.JSON(200, gin.H{"deregistered": true})
	})

	// ── Decisions ────────────────────────────────────────────────────
	api.GET("/decisions", func(c *gin.Context) {
		b.state.mu.RLock()
		decisions := b.state.Decisions
		if decisions == nil { decisions = []*Decision{} }
		b.state.mu.RUnlock()
		c.JSON(200, gin.H{"decisions": decisions, "total": len(decisions)})
	})

	api.POST("/decisions", func(c *gin.Context) {
		var body struct {
			DecisionType string                 `json:"decision_type" binding:"required"`
			AgentID      string                 `json:"agent_id"`
			ServiceName  string                 `json:"service_name"`
			Reason       string                 `json:"reason"`
			Action       map[string]interface{} `json:"action"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		go b.issueDecision(body.DecisionType, body.AgentID, body.ServiceName, body.Reason, body.Action)
		c.JSON(202, gin.H{"queued": true, "decision_type": body.DecisionType})
	})

	// ── Node registry ─────────────────────────────────────────────────
	api.GET("/nodes", func(c *gin.Context) {
		rows, err := b.db.QueryContext(c.Request.Context(),
			`SELECT node_id, hostname, region, cpu_cores, memory_gb, cpu_percent, memory_percent, agent_count, status, last_seen FROM node_registry ORDER BY last_seen DESC`)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var nodes []NodeRecord
		for rows.Next() {
			var n NodeRecord
			rows.Scan(&n.NodeID, &n.Hostname, &n.Region, &n.CPUCores, &n.MemoryGB,
				&n.CPUPercent, &n.MemoryPercent, &n.AgentCount, &n.Status, &n.LastSeen)
			nodes = append(nodes, n)
		}
		if nodes == nil { nodes = []NodeRecord{} }
		c.JSON(200, nodes)
	})

	api.POST("/nodes/heartbeat", func(c *gin.Context) {
		var body NodeRecord
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		body.LastSeen = time.Now()
		b.db.ExecContext(c.Request.Context(), `
			INSERT INTO node_registry (node_id, hostname, region, cpu_cores, memory_gb, cpu_percent, memory_percent, agent_count, status, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'healthy', NOW())
			ON CONFLICT (node_id) DO UPDATE SET
				cpu_percent=EXCLUDED.cpu_percent, memory_percent=EXCLUDED.memory_percent,
				agent_count=EXCLUDED.agent_count, status='healthy', last_seen=NOW()`,
			body.NodeID, body.Hostname, body.Region, body.CPUCores, body.MemoryGB,
			body.CPUPercent, body.MemoryPercent, body.AgentCount)
		b.state.mu.Lock()
		b.state.Nodes[body.NodeID] = &body
		b.state.mu.Unlock()
		c.JSON(200, gin.H{"ok": true})
	})

	// ── Scaling decisions ─────────────────────────────────────────────
	api.POST("/scale", func(c *gin.Context) {
		var body struct {
			AgentID    string `json:"agent_id" binding:"required"`
			Direction  string `json:"direction" binding:"required"` // up|down
			Replicas   int    `json:"replicas"`
			Reason     string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		if body.Replicas <= 0 { body.Replicas = 1 }
		action := map[string]interface{}{
			"agent_id": body.AgentID, "replicas": body.Replicas, "direction": body.Direction,
		}
		go b.issueDecision("scale_"+body.Direction, body.AgentID, "", body.Reason, action)
		c.JSON(202, gin.H{"scale_queued": true, "direction": body.Direction, "replicas": body.Replicas})
	})

	// ── Force reconcile ───────────────────────────────────────────────
	api.POST("/reconcile", func(c *gin.Context) {
		go b.reconcile(context.Background())
		c.JSON(202, gin.H{"reconcile_triggered": true})
	})

	// ── Placement advisor ─────────────────────────────────────────────
	api.POST("/placement", func(c *gin.Context) {
		var body struct {
			CPURequest    float64 `json:"cpu_request"`
			MemoryRequest float64 `json:"memory_request_mb"`
			Region        string  `json:"region"`
		}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }

		// Find least loaded node in requested region
		b.state.mu.RLock()
		var bestNode *NodeRecord
		for _, n := range b.state.Nodes {
			if n.Status != "healthy" { continue }
			if body.Region != "" && n.Region != body.Region { continue }
			if bestNode == nil || n.CPUPercent < bestNode.CPUPercent { bestNode = n }
		}
		b.state.mu.RUnlock()

		if bestNode == nil {
			c.JSON(200, gin.H{"node_id": "default", "region": body.Region, "reason": "no_nodes_tracked"})
			return
		}
		c.JSON(200, gin.H{
			"node_id":     bestNode.NodeID,
			"region":      bestNode.Region,
			"cpu_free":    100 - bestNode.CPUPercent,
			"memory_free": (1 - bestNode.MemoryPercent/100) * bestNode.MemoryGB * 1024,
		})
	})

	// ── Heartbeat broadcast endpoint ──────────────────────────────────
	r.GET("/brain/heartbeat", func(c *gin.Context) {
		c.JSON(200, b.state.statusSummary())
	})

	return r
}

// ═══════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("starting control-brain")

	cfg := loadConfig()

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil { logger.Error("db open", "error", err); os.Exit(1) }
	defer db.Close()
	for i := 0; i < 15; i++ {
		if err = db.Ping(); err == nil { break }
		logger.Info("waiting for postgres...", "attempt", i+1)
		time.Sleep(3 * time.Second)
	}
	if err != nil { logger.Error("postgres never ready", "error", err); os.Exit(1) }
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	nc, err := natsgo.Connect(cfg.NatsURL,
		natsgo.RetryOnFailedConnect(true),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(2*time.Second),
	)
	if err != nil {
		logger.Warn("nats unavailable — continuing without event streaming", "error", err)
	}
	defer func() { if nc != nil { nc.Drain() } }()

	brain := NewBrain(cfg, db, nc)
	brain.warmState(context.Background())

	// Subscribe to all platform events
	if nc != nil {
		nc.Subscribe("agents.events", brain.handleNATSEvent)
		nc.Subscribe("metrics.stream", brain.handleNATSEvent)
		nc.Subscribe("brain.commands", func(msg *natsgo.Msg) {
			brain.logger.Debug("brain command received")
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start control loops
	go brain.reconcileLoop(ctx)
	go brain.healthCheckLoop(ctx)

	// Heartbeat broadcast
	go func() {
		ticker := time.NewTicker(cfg.HeartbeatInterval)
		defer ticker.Stop()
		for range ticker.C {
			if nc == nil { continue }
			nc.Publish("brain.heartbeat", mustJSON(map[string]interface{}{
				"source":    "control-brain",
				"timestamp": time.Now().UTC(),
				"summary":   brain.state.statusSummary(),
			}))
		}
	}()

	router := brain.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("control-brain HTTP listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	logger.Info("shutting down control-brain")
	shutCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	srv.Shutdown(shutCtx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}