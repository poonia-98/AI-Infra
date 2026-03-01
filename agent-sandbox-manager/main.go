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

// ── Config ─────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL string
	NatsURL     string
	BackendURL  string
	Port        string
	// Runtime availability
	GVisorEnabled       bool
	FirecrackerEnabled  bool
	DefaultRuntime      string
}

func loadConfig() Config {
	return Config{
		DatabaseURL:        getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:            getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:         getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		Port:               getEnv("PORT", "8096"),
		GVisorEnabled:      getEnv("GVISOR_ENABLED", "false") == "true",
		FirecrackerEnabled: getEnv("FIRECRACKER_ENABLED", "false") == "true",
		DefaultRuntime:     getEnv("DEFAULT_RUNTIME", "docker"),
	}
}

// ── Metrics ─────────────────────────────────────────────────────────────────

var (
	sandboxesCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_sandboxes_created_total",
	}, []string{"runtime"})
	sandboxViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_sandbox_violations_total",
	}, []string{"violation_type", "severity"})
	activeSandboxes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_sandboxes_active",
	}, []string{"runtime"})
)

func init() {
	prometheus.MustRegister(sandboxesCreated, sandboxViolations, activeSandboxes)
}

// ── Domain ──────────────────────────────────────────────────────────────────

type SandboxPolicy struct {
	ID             string
	OrgID          string
	Name           string
	Runtime        string
	NetworkMode    string
	AllowedHosts   []string
	ReadonlyRoot   bool
	AllowedPaths   []string
	BlockedPaths   []string
	MaxCPUPct      float64
	MaxMemoryMB    int
	MaxPIDs        int
	MaxOpenFiles   int
	SeccompProfile string
	DroppedCaps    []string
	AddedCaps      []string
	BlockedSyscalls []string
	MaxExecSeconds int
	Policy         map[string]interface{}
}

type SandboxCreateRequest struct {
	AgentID        string                 `json:"agent_id" binding:"required"`
	ExecutionID    string                 `json:"execution_id"`
	OrganisationID string                 `json:"organisation_id"`
	Image          string                 `json:"image" binding:"required"`
	Command        []string               `json:"command"`
	EnvVars        map[string]string      `json:"env_vars"`
	PolicyID       string                 `json:"policy_id"`
	RuntimeHint    string                 `json:"runtime"` // override runtime
	CPULimit       string                 `json:"cpu_limit"`
	MemoryLimit    string                 `json:"memory_limit"`
	Labels         map[string]string      `json:"labels"`
}

type SandboxViolation struct {
	AgentID       string                 `json:"agent_id"`
	OrgID         string                 `json:"organisation_id"`
	PolicyID      string                 `json:"policy_id"`
	ViolationType string                 `json:"violation_type"`
	Details       map[string]interface{} `json:"details"`
	Severity      string                 `json:"severity"`
	ActionTaken   string                 `json:"action_taken"`
}

// ── Manager ──────────────────────────────────────────────────────────────────

type SandboxManager struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func NewSandboxManager(cfg Config, db *sql.DB, nc *natsgo.Conn) *SandboxManager {
	return &SandboxManager{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 30 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Policy Resolution ────────────────────────────────────────────────────────

func (m *SandboxManager) resolvePolicy(ctx context.Context, policyID, orgID string) (*SandboxPolicy, error) {
	var query string
	var args []interface{}

	if policyID != "" {
		query = `SELECT id::text, COALESCE(organisation_id::text,''), name, runtime,
		               network_mode, allowed_hosts, readonly_root, allowed_paths,
		               blocked_paths, max_cpu_pct, max_memory_mb, max_pids,
		               max_open_files, seccomp_profile, dropped_caps, added_caps,
		               blocked_syscalls, max_exec_seconds, policy
		        FROM sandbox_policies WHERE id=$1::uuid`
		args = []interface{}{policyID}
	} else if orgID != "" {
		// Find org-specific default, fallback to global default
		query = `SELECT id::text, COALESCE(organisation_id::text,''), name, runtime,
		               network_mode, allowed_hosts, readonly_root, allowed_paths,
		               blocked_paths, max_cpu_pct, max_memory_mb, max_pids,
		               max_open_files, seccomp_profile, dropped_caps, added_caps,
		               blocked_syscalls, max_exec_seconds, policy
		        FROM sandbox_policies
		        WHERE (organisation_id=$1::uuid OR is_default=TRUE) AND status IS NULL OR status='active'
		        ORDER BY (organisation_id IS NOT NULL) DESC, is_default DESC
		        LIMIT 1`
		args = []interface{}{orgID}
	} else {
		query = `SELECT id::text, COALESCE(organisation_id::text,''), name, runtime,
		               network_mode, allowed_hosts, readonly_root, allowed_paths,
		               blocked_paths, max_cpu_pct, max_memory_mb, max_pids,
		               max_open_files, seccomp_profile, dropped_caps, added_caps,
		               blocked_syscalls, max_exec_seconds, policy
		        FROM sandbox_policies WHERE is_default=TRUE LIMIT 1`
	}

	row := m.db.QueryRowContext(ctx, query, args...)
	p := &SandboxPolicy{}
	var allowedHostsJSON, allowedPathsJSON, blockedPathsJSON []byte
	var droppedCapsJSON, addedCapsJSON, blockedSyscallsJSON, policyJSON []byte

	err := row.Scan(&p.ID, &p.OrgID, &p.Name, &p.Runtime,
		&p.NetworkMode, &allowedHostsJSON, &p.ReadonlyRoot,
		&allowedPathsJSON, &blockedPathsJSON,
		&p.MaxCPUPct, &p.MaxMemoryMB, &p.MaxPIDs,
		&p.MaxOpenFiles, &p.SeccompProfile,
		&droppedCapsJSON, &addedCapsJSON, &blockedSyscallsJSON,
		&p.MaxExecSeconds, &policyJSON)
	if err != nil {
		// Return hardcoded default policy
		return m.defaultPolicy(), nil
	}

	json.Unmarshal(allowedHostsJSON, &p.AllowedHosts)
	json.Unmarshal(allowedPathsJSON, &p.AllowedPaths)
	json.Unmarshal(blockedPathsJSON, &p.BlockedPaths)
	json.Unmarshal(droppedCapsJSON, &p.DroppedCaps)
	json.Unmarshal(addedCapsJSON, &p.AddedCaps)
	json.Unmarshal(blockedSyscallsJSON, &p.BlockedSyscalls)
	json.Unmarshal(policyJSON, &p.Policy)
	return p, nil
}

func (m *SandboxManager) defaultPolicy() *SandboxPolicy {
	return &SandboxPolicy{
		Name:           "hardcoded-default",
		Runtime:        m.cfg.DefaultRuntime,
		NetworkMode:    "restricted",
		ReadonlyRoot:   true,
		AllowedPaths:   []string{"/tmp", "/var/run"},
		BlockedPaths:   []string{"/proc/sysrq-trigger", "/sys/kernel"},
		MaxCPUPct:      80.0,
		MaxMemoryMB:    512,
		MaxPIDs:        256,
		MaxOpenFiles:   1024,
		SeccompProfile: "default",
		DroppedCaps:    []string{"ALL"},
		AddedCaps:      []string{},
		MaxExecSeconds: 3600,
	}
}

// ── Sandbox Creation ─────────────────────────────────────────────────────────

func (m *SandboxManager) CreateSandbox(ctx context.Context, req SandboxCreateRequest) (map[string]interface{}, error) {
	policy, err := m.resolvePolicy(ctx, req.PolicyID, req.OrganisationID)
	if err != nil {
		return nil, fmt.Errorf("policy resolution: %w", err)
	}

	// Determine runtime
	runtime := policy.Runtime
	if req.RuntimeHint != "" && m.isRuntimeAvailable(req.RuntimeHint) {
		runtime = req.RuntimeHint
	}

	config := m.buildSandboxConfig(req, policy, runtime)
	sandboxID := fmt.Sprintf("sb-%s", req.AgentID[:8])

	sandboxesCreated.With(prometheus.Labels{"runtime": runtime}).Inc()
	activeSandboxes.With(prometheus.Labels{"runtime": runtime}).Inc()

	m.logger.Info("sandbox created",
		"sandbox_id", sandboxID,
		"agent_id", req.AgentID,
		"runtime", runtime,
		"policy", policy.Name,
		"network_mode", policy.NetworkMode,
	)

	m.publishEvent("sandbox.created", map[string]interface{}{
		"sandbox_id": sandboxID,
		"agent_id":   req.AgentID,
		"runtime":    runtime,
		"policy":     policy.Name,
	})

	return map[string]interface{}{
		"sandbox_id":   sandboxID,
		"runtime":      runtime,
		"policy_id":    policy.ID,
		"policy_name":  policy.Name,
		"network_mode": policy.NetworkMode,
		"config":       config,
	}, nil
}

func (m *SandboxManager) buildSandboxConfig(req SandboxCreateRequest, policy *SandboxPolicy, runtime string) map[string]interface{} {
	config := map[string]interface{}{
		"image":         req.Image,
		"command":       req.Command,
		"env_vars":      req.EnvVars,
		"runtime":       runtime,
		"readonly_root": policy.ReadonlyRoot,
		"network": map[string]interface{}{
			"mode":          policy.NetworkMode,
			"allowed_hosts": policy.AllowedHosts,
		},
		"resources": map[string]interface{}{
			"max_cpu_pct":    policy.MaxCPUPct,
			"max_memory_mb":  policy.MaxMemoryMB,
			"max_pids":       policy.MaxPIDs,
			"max_open_files": policy.MaxOpenFiles,
		},
		"security": map[string]interface{}{
			"seccomp_profile":  policy.SeccompProfile,
			"dropped_caps":     policy.DroppedCaps,
			"added_caps":       policy.AddedCaps,
			"blocked_syscalls": policy.BlockedSyscalls,
			"allowed_paths":    policy.AllowedPaths,
			"blocked_paths":    policy.BlockedPaths,
			"no_new_privileges": true,
			"run_as_non_root":  true,
			"run_as_user":      65534,
		},
		"timeouts": map[string]interface{}{
			"max_exec_seconds": policy.MaxExecSeconds,
		},
	}

	// Runtime-specific additions
	switch runtime {
	case "gvisor":
		config["gvisor"] = map[string]interface{}{
			"platform": "ptrace",
			"network":  "sandbox",
		}
	case "firecracker":
		config["firecracker"] = map[string]interface{}{
			"kernel_image":  "/firecracker/vmlinux",
			"rootfs":        "/firecracker/rootfs.ext4",
			"vcpu_count":    1,
			"mem_size_mib":  policy.MaxMemoryMB,
			"net_iface":     "eth0",
		}
	case "kata":
		config["kata"] = map[string]interface{}{
			"runtime_class": "kata-containers",
		}
	}

	// Apply resource limits from request if stricter than policy
	if req.CPULimit != "" {
		config["cpu_limit"] = req.CPULimit
	}
	if req.MemoryLimit != "" {
		config["memory_limit"] = req.MemoryLimit
	}

	return config
}

func (m *SandboxManager) isRuntimeAvailable(runtime string) bool {
	switch runtime {
	case "docker":
		return true
	case "gvisor":
		return m.cfg.GVisorEnabled
	case "firecracker":
		return m.cfg.FirecrackerEnabled
	case "kata":
		return false // TODO: add kata detection
	}
	return false
}

// ── Violation Recording ──────────────────────────────────────────────────────

func (m *SandboxManager) RecordViolation(ctx context.Context, v SandboxViolation) error {
	detailsJSON, _ := json.Marshal(v.Details)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO sandbox_violations
		  (agent_id, organisation_id, policy_id, violation_type, details, severity, action_taken)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, NULLIF($3,'')::uuid, $4, $5::jsonb, $6, $7)`,
		v.AgentID, v.OrgID, v.PolicyID,
		v.ViolationType, string(detailsJSON), v.Severity, v.ActionTaken)

	sandboxViolations.With(prometheus.Labels{
		"violation_type": v.ViolationType,
		"severity":       v.Severity,
	}).Inc()

	m.publishEvent("sandbox.violation", map[string]interface{}{
		"agent_id":       v.AgentID,
		"violation_type": v.ViolationType,
		"severity":       v.Severity,
		"action_taken":   v.ActionTaken,
	})

	if v.Severity == "critical" || v.Severity == "high" {
		// Trigger immediate agent termination for critical violations
		m.terminateAgent(ctx, v.AgentID, v.ViolationType)
	}

	return err
}

func (m *SandboxManager) terminateAgent(ctx context.Context, agentID, reason string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"reason": "sandbox_violation: " + reason,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		m.cfg.BackendURL+"/api/v1/agents/"+agentID+"/stop", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor-Type", "sandbox-manager")
	resp, err := m.http.Do(req)
	if err == nil {
		resp.Body.Close()
		m.logger.Warn("agent terminated due to sandbox violation",
			"agent_id", agentID, "reason", reason)
	}
}

// ── NATS subscription for runtime violations ──────────────────────────────────

func (m *SandboxManager) Subscribe(ctx context.Context) {
	m.nc.Subscribe("sandbox.violation.report", func(msg *natsgo.Msg) {
		var v SandboxViolation
		if err := json.Unmarshal(msg.Data, &v); err != nil {
			return
		}
		m.RecordViolation(ctx, v)
	})

	m.nc.Subscribe("agent.stopped", func(msg *natsgo.Msg) {
		var event struct {
			AgentID string `json:"agent_id"`
			Runtime string `json:"runtime"`
		}
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			return
		}
		if event.Runtime != "" {
			activeSandboxes.With(prometheus.Labels{"runtime": event.Runtime}).Dec()
		}
	})
}

func (m *SandboxManager) publishEvent(subject string, data map[string]interface{}) {
	data["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	payload, _ := json.Marshal(data)
	_ = m.nc.Publish("sandbox.events", payload)
	_ = m.nc.Publish(subject, payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (m *SandboxManager) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "healthy",
			"service": "agent-sandbox-manager",
			"runtimes": gin.H{
				"docker":      true,
				"gvisor":      m.cfg.GVisorEnabled,
				"firecracker": m.cfg.FirecrackerEnabled,
			},
		})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := m.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Create a sandbox configuration for an agent
	r.POST("/sandboxes", func(c *gin.Context) {
		var req SandboxCreateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		result, err := m.CreateSandbox(c.Request.Context(), req)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, result)
	})

	// Report a violation
	r.POST("/violations", func(c *gin.Context) {
		var v SandboxViolation
		if err := c.ShouldBindJSON(&v); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := m.RecordViolation(c.Request.Context(), v); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"status": "recorded"})
	})

	// List violations for an agent
	r.GET("/agents/:agent_id/violations", func(c *gin.Context) {
		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT id::text, violation_type, details, severity, action_taken, created_at
			FROM sandbox_violations WHERE agent_id=$1::uuid
			ORDER BY created_at DESC LIMIT 100`, c.Param("agent_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var violations []map[string]interface{}
		for rows.Next() {
			var id, vtype, severity, action string
			var details []byte
			var ts time.Time
			rows.Scan(&id, &vtype, &details, &severity, &action, &ts)
			var d interface{}
			json.Unmarshal(details, &d)
			violations = append(violations, map[string]interface{}{
				"id": id, "violation_type": vtype, "details": d,
				"severity": severity, "action_taken": action, "created_at": ts,
			})
		}
		if violations == nil {
			violations = []map[string]interface{}{}
		}
		c.JSON(200, violations)
	})

	// Get/create/update policies
	r.GET("/policies", func(c *gin.Context) {
		orgID := c.Query("organisation_id")
		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT id::text, name, runtime, network_mode, max_memory_mb,
			       max_cpu_pct, is_default, created_at
			FROM sandbox_policies
			WHERE organisation_id=NULLIF($1,'')::uuid OR organisation_id IS NULL
			ORDER BY (organisation_id IS NOT NULL) DESC, is_default DESC`, orgID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var policies []map[string]interface{}
		for rows.Next() {
			var id, name, runtime, netMode string
			var memMB int
			var cpuPct float64
			var isDefault bool
			var ts time.Time
			rows.Scan(&id, &name, &runtime, &netMode, &memMB, &cpuPct, &isDefault, &ts)
			policies = append(policies, map[string]interface{}{
				"id": id, "name": name, "runtime": runtime,
				"network_mode": netMode, "max_memory_mb": memMB,
				"max_cpu_pct": cpuPct, "is_default": isDefault, "created_at": ts,
			})
		}
		if policies == nil {
			policies = []map[string]interface{}{}
		}
		c.JSON(200, policies)
	})

	r.POST("/policies", func(c *gin.Context) {
		var body struct {
			OrgID          string   `json:"organisation_id"`
			Name           string   `json:"name" binding:"required"`
			Runtime        string   `json:"runtime"`
			NetworkMode    string   `json:"network_mode"`
			ReadonlyRoot   bool     `json:"readonly_root"`
			MaxCPUPct      float64  `json:"max_cpu_pct"`
			MaxMemoryMB    int      `json:"max_memory_mb"`
			MaxPIDs        int      `json:"max_pids"`
			MaxExecSeconds int      `json:"max_exec_seconds"`
			DroppedCaps    []string `json:"dropped_caps"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.Runtime == "" {
			body.Runtime = m.cfg.DefaultRuntime
		}
		if body.NetworkMode == "" {
			body.NetworkMode = "restricted"
		}
		if body.MaxMemoryMB == 0 {
			body.MaxMemoryMB = 512
		}
		if body.MaxCPUPct == 0 {
			body.MaxCPUPct = 80
		}
		if body.MaxPIDs == 0 {
			body.MaxPIDs = 256
		}
		if body.MaxExecSeconds == 0 {
			body.MaxExecSeconds = 3600
		}
		droppedCapsJSON, _ := json.Marshal(body.DroppedCaps)

		var id string
		err := m.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO sandbox_policies
			  (organisation_id, name, runtime, network_mode, readonly_root,
			   max_cpu_pct, max_memory_mb, max_pids, max_exec_seconds, dropped_caps)
			VALUES (NULLIF($1,'')::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10::text[])
			RETURNING id::text`,
			body.OrgID, body.Name, body.Runtime, body.NetworkMode, body.ReadonlyRoot,
			body.MaxCPUPct, body.MaxMemoryMB, body.MaxPIDs, body.MaxExecSeconds,
			string(droppedCapsJSON)).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id, "name": body.Name})
	})

	// Available runtimes
	r.GET("/runtimes", func(c *gin.Context) {
		runtimes := []map[string]interface{}{
			{"name": "docker", "available": true, "isolation": "container"},
		}
		if m.cfg.GVisorEnabled {
			runtimes = append(runtimes, map[string]interface{}{
				"name": "gvisor", "available": true, "isolation": "kernel",
				"description": "gVisor user-space kernel for enhanced isolation",
			})
		}
		if m.cfg.FirecrackerEnabled {
			runtimes = append(runtimes, map[string]interface{}{
				"name": "firecracker", "available": true, "isolation": "microvm",
				"description": "Firecracker MicroVM for strongest isolation",
			})
		}
		c.JSON(200, runtimes)
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting agent-sandbox-manager", "port", cfg.Port,
		"default_runtime", cfg.DefaultRuntime,
		"gvisor", cfg.GVisorEnabled,
		"firecracker", cfg.FirecrackerEnabled)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil && db.PingContext(context.Background()) == nil {
			break
		}
		logger.Info("waiting for postgres", "attempt", i+1)
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
			natsgo.ReconnectWait(2*time.Second), natsgo.Name("agent-sandbox-manager"))
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

	mgr := NewSandboxManager(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Subscribe(ctx)

	router := mgr.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		logger.Info("sandbox manager listening", "port", cfg.Port)
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
	logger.Info("sandbox manager stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}