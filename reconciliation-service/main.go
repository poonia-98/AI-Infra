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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
)

// ── Configuration ────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL       string
	NatsURL           string
	ExecutorURL       string
	ReconcileInterval time.Duration
	NodeID            string
	NodeHeartbeatTTL  time.Duration
	StuckStartingTTL  time.Duration
	Port              string
}

func loadConfig() Config {
	interval, _ := time.ParseDuration(getEnv("RECONCILE_INTERVAL", "5s"))
	heartbeatTTL, _ := time.ParseDuration(getEnv("NODE_HEARTBEAT_TTL", "30s"))
	stuckTTL, _ := time.ParseDuration(getEnv("STUCK_STARTING_TTL", "120s"))
	return Config{
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:           getEnv("NATS_URL", "nats://nats:4222"),
		ExecutorURL:       getEnv("EXECUTOR_URL", "http://executor:8081"),
		ReconcileInterval: interval,
		NodeID:            getEnv("NODE_ID", "executor-default"),
		NodeHeartbeatTTL:  heartbeatTTL,
		StuckStartingTTL:  stuckTTL,
		Port:              getEnv("PORT", "8085"),
	}
}

// ── Domain types ─────────────────────────────────────────────────────────────

type AgentRecord struct {
	ID            string
	Name          string
	Status        string
	DesiredState  string
	ContainerID   string
	NodeID        string
	RestartPolicy string
	RestartCount  int
}

type ExecutionRecord struct {
	ID            string
	AgentID       string
	Status        string
	DesiredState  string
	ActualState   string
	ContainerID   string
	NodeID        string
	RestartPolicy string
	RestartCount  int
	StartedAt     *time.Time
}

type ContainerRecord struct {
	ID      string
	AgentID string
	Name    string
	State   string
	Status  string
}

type NodeRecord struct {
	NodeID        string
	Status        string
	LastHeartbeat time.Time
}

type ReconcileCase string

const (
	CaseRunningNoCont   ReconcileCase = "running_execution_no_container"
	CaseOrphanContainer ReconcileCase = "orphan_container"
	CaseStoppedRunning  ReconcileCase = "stopped_execution_container_running"
	CaseNodeDead        ReconcileCase = "node_dead_execution_running"
	CaseStuckStarting   ReconcileCase = "execution_stuck_starting"
	CaseExitedRunning   ReconcileCase = "container_exited_execution_running"
)

// ── Service ──────────────────────────────────────────────────────────────────

type Reconciler struct {
	cfg    Config
	db     *sql.DB
	dc     *client.Client
	nc     *natsgo.Conn
	logger *slog.Logger
	http   *http.Client
}

func NewReconciler(cfg Config, db *sql.DB, dc *client.Client, nc *natsgo.Conn) *Reconciler {
	return &Reconciler{
		cfg:    cfg,
		db:     db,
		dc:     dc,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

// ── Main reconciliation loop ─────────────────────────────────────────────────

func (r *Reconciler) Run(ctx context.Context) {
	r.logger.Info("reconciliation loop started", "interval", r.cfg.ReconcileInterval)

	// ── Leader election: only run reconcile loop if we are the leader ──
	hostname, _ := os.Hostname()
	leaderID := r.cfg.NodeID + "@" + hostname

	var isLeader bool
	ttl := 30 * time.Second
	electTicker := time.NewTicker(ttl / 3)
	defer electTicker.Stop()

	tryElect := func() {
		ttlSecs := int(ttl.Seconds())
		res, err := r.db.ExecContext(ctx, `
			INSERT INTO service_leaders (service_name, leader_id, acquired_at, ttl_seconds)
			VALUES ('reconciliation', $1, NOW(), $2)
			ON CONFLICT (service_name) DO UPDATE
			  SET leader_id=$1, acquired_at=NOW(), ttl_seconds=$2
			WHERE service_leaders.leader_id=$1
			   OR NOW()-service_leaders.acquired_at > (service_leaders.ttl_seconds*INTERVAL '1 second')`,
			leaderID, ttlSecs)
		if err == nil {
			rows, _ := res.RowsAffected()
			wasLeader := isLeader
			isLeader = rows > 0
			if !wasLeader && isLeader {
				r.logger.Info("became reconciliation leader", "leader_id", leaderID)
			} else if wasLeader && !isLeader {
				r.logger.Info("lost reconciliation leadership")
			}
		} else {
			r.logger.Debug("leader election query failed (table may not exist yet)", "error", err)
			isLeader = true // fallback: run without election if table missing
		}
	}

	tryElect()
	ticker := time.NewTicker(r.cfg.ReconcileInterval)
	defer ticker.Stop()

	// First reconcile immediately on start
	r.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("reconciliation loop stopped")
			return
		case <-electTicker.C:
			tryElect()
		case <-ticker.C:
			if isLeader {
				r.reconcile(ctx)
			}
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) {
	start := time.Now()
	defer func() {
		r.logger.Info("reconcile cycle complete", "duration_ms", time.Since(start).Milliseconds())
	}()

	// Fetch all state sources in parallel
	containers, err := r.fetchContainers(ctx)
	if err != nil {
		r.logger.Error("failed to fetch containers", "error", err)
		return
	}

	agents, err := r.fetchAgents(ctx)
	if err != nil {
		r.logger.Error("failed to fetch agents", "error", err)
		return
	}

	executions, err := r.fetchActiveExecutions(ctx)
	if err != nil {
		r.logger.Error("failed to fetch executions", "error", err)
		return
	}

	nodes, err := r.fetchNodes(ctx)
	if err != nil {
		r.logger.Warn("failed to fetch nodes", "error", err)
		nodes = nil
	}

	// Build lookup maps
	containerByID := make(map[string]ContainerRecord)
	containerByAgent := make(map[string]ContainerRecord)
	for _, c := range containers {
		containerByID[c.ID] = c
		containerByAgent[c.AgentID] = c
	}

	agentByID := make(map[string]AgentRecord)
	for _, a := range agents {
		agentByID[a.ID] = a
	}

	executionByContainer := make(map[string]ExecutionRecord)
	for _, e := range executions {
		if e.ContainerID != "" {
			executionByContainer[e.ContainerID] = e
		}
	}

	// ── CASE 1: Execution RUNNING but container missing ─────────────────
	for _, exec := range executions {
		if exec.Status != "running" && exec.Status != "starting" {
			continue
		}
		if exec.ContainerID == "" {
			continue
		}
		if _, exists := containerByID[exec.ContainerID]; !exists {
			r.handleRunningNoCont(ctx, exec)
		}
	}

	// ── CASE 2: Container exists but no execution ────────────────────────
	for _, ctr := range containers {
		if ctr.State != "running" {
			continue
		}
		if _, exists := executionByContainer[ctr.ID]; !exists {
			if _, agentExists := agentByID[ctr.AgentID]; !agentExists {
				r.handleOrphanContainer(ctx, ctr)
			}
		}
	}

	// ── CASE 3: Execution STOPPED but container still running ─────────────
	for _, exec := range executions {
		if exec.Status != "stopped" && exec.DesiredState != "stopped" {
			continue
		}
		if exec.ContainerID == "" {
			continue
		}
		if ctr, exists := containerByID[exec.ContainerID]; exists && ctr.State == "running" {
			r.handleStoppedButRunning(ctx, exec, ctr)
		}
	}

	// ── CASE 4: Dead node with running executions ──────────────────────
	deadNodes := make(map[string]bool)
	for _, node := range nodes {
		if time.Since(node.LastHeartbeat) > r.cfg.NodeHeartbeatTTL {
			deadNodes[node.NodeID] = true
			r.handleDeadNode(ctx, node)
		}
	}
	for _, exec := range executions {
		if exec.NodeID != "" && deadNodes[exec.NodeID] && exec.Status == "running" {
			r.handleNodeDeadExecution(ctx, exec)
		}
	}

	// ── CASE 5: Execution stuck in STARTING ───────────────────────────
	for _, exec := range executions {
		if exec.Status != "starting" {
			continue
		}
		if exec.StartedAt != nil && time.Since(*exec.StartedAt) > r.cfg.StuckStartingTTL {
			r.handleStuckStarting(ctx, exec)
		}
	}

	// ── CASE 6: Container exited but DB shows RUNNING ──────────────────
	for _, ctr := range containers {
		if ctr.State == "running" {
			continue
		}
		if exec, exists := executionByContainer[ctr.ID]; exists && exec.Status == "running" {
			r.handleExitedContainer(ctx, exec, ctr)
		}
	}

	// ── Desired state enforcement for agents ──────────────────────────
	for _, agent := range agents {
		r.enforceAgentDesiredState(ctx, agent, containerByAgent)
	}
}

// ── Case handlers ─────────────────────────────────────────────────────────────

func (r *Reconciler) handleRunningNoCont(ctx context.Context, exec ExecutionRecord) {
	r.logger.Warn("CASE 1: execution running but container missing",
		"execution_id", exec.ID, "agent_id", exec.AgentID, "container_id", exec.ContainerID)

	r.auditEvent(ctx, exec.AgentID, exec.ID, CaseRunningNoCont, "mark_failed",
		map[string]interface{}{"status": exec.Status},
		map[string]interface{}{"status": "failed"},
	)

	if err := r.markExecutionFailed(ctx, exec.ID, "container not found during reconciliation"); err != nil {
		r.logger.Error("failed to mark execution failed", "error", err, "execution_id", exec.ID)
		return
	}
	if err := r.updateAgentStatus(ctx, exec.AgentID, "failed"); err != nil {
		r.logger.Error("failed to update agent status", "error", err)
	}

	r.publishEvent("agent.execution.failed", map[string]interface{}{
		"execution_id": exec.ID, "agent_id": exec.AgentID,
		"reason": "container_missing", "timestamp": time.Now().UTC(),
	})

	r.applyRestartPolicy(ctx, exec)
}

func (r *Reconciler) handleOrphanContainer(ctx context.Context, ctr ContainerRecord) {
	r.logger.Warn("CASE 2: orphan container found",
		"container_id", ctr.ID[:min(12, len(ctr.ID))], "agent_id", ctr.AgentID)

	r.auditEvent(ctx, ctr.AgentID, "", CaseOrphanContainer, "stop_container",
		map[string]interface{}{"container_state": ctr.State},
		map[string]interface{}{"container_state": "stopped"},
	)

	timeout := 10
	_ = r.dc.ContainerStop(ctx, ctr.ID, container.StopOptions{Timeout: &timeout})
	_ = r.dc.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true})

	r.publishEvent("agent.execution.orphaned", map[string]interface{}{
		"container_id": ctr.ID, "agent_id": ctr.AgentID,
		"timestamp": time.Now().UTC(),
	})
}

func (r *Reconciler) handleStoppedButRunning(ctx context.Context, exec ExecutionRecord, ctr ContainerRecord) {
	r.logger.Info("CASE 3: stopped execution but container still running — stopping",
		"execution_id", exec.ID, "container_id", ctr.ID[:min(12, len(ctr.ID))])

	r.auditEvent(ctx, exec.AgentID, exec.ID, CaseStoppedRunning, "stop_container",
		map[string]interface{}{"container_state": "running"},
		map[string]interface{}{"container_state": "stopped"},
	)

	r.stopContainerViaExecutor(ctx, exec.AgentID, ctr.ID)
}

func (r *Reconciler) handleDeadNode(ctx context.Context, node NodeRecord) {
	r.logger.Warn("CASE 4: node dead",
		"node_id", node.NodeID, "last_heartbeat", node.LastHeartbeat)

	_, err := r.db.ExecContext(ctx,
		`UPDATE executor_nodes SET status='dead' WHERE node_id=$1 AND status!='dead'`,
		node.NodeID)
	if err != nil {
		r.logger.Error("failed to mark node dead", "error", err)
	}

	r.publishEvent("agent.node.failed", map[string]interface{}{
		"node_id": node.NodeID, "last_heartbeat": node.LastHeartbeat,
		"timestamp": time.Now().UTC(),
	})
}

func (r *Reconciler) handleNodeDeadExecution(ctx context.Context, exec ExecutionRecord) {
	r.logger.Warn("CASE 4b: node dead with running execution",
		"execution_id", exec.ID, "node_id", exec.NodeID)

	r.auditEvent(ctx, exec.AgentID, exec.ID, CaseNodeDead, "mark_unknown",
		map[string]interface{}{"status": exec.Status, "node_id": exec.NodeID},
		map[string]interface{}{"status": "failed"},
	)

	_ = r.markExecutionFailed(ctx, exec.ID, fmt.Sprintf("node %s is dead", exec.NodeID))
	r.applyRestartPolicy(ctx, exec)
}

func (r *Reconciler) handleStuckStarting(ctx context.Context, exec ExecutionRecord) {
	r.logger.Warn("CASE 5: execution stuck in starting state",
		"execution_id", exec.ID, "started_at", exec.StartedAt)

	r.auditEvent(ctx, exec.AgentID, exec.ID, CaseStuckStarting, "mark_failed",
		map[string]interface{}{"status": "starting"},
		map[string]interface{}{"status": "failed"},
	)

	_ = r.markExecutionFailed(ctx, exec.ID, "execution stuck in starting state (timeout)")
	_ = r.updateAgentStatus(ctx, exec.AgentID, "failed")
	r.publishEvent("agent.execution.failed", map[string]interface{}{
		"execution_id": exec.ID, "agent_id": exec.AgentID,
		"reason": "stuck_starting", "timestamp": time.Now().UTC(),
	})
}

func (r *Reconciler) handleExitedContainer(ctx context.Context, exec ExecutionRecord, ctr ContainerRecord) {
	r.logger.Info("CASE 6: container exited but execution still running",
		"execution_id", exec.ID, "container_id", ctr.ID[:min(12, len(ctr.ID))], "state", ctr.State)

	inspect, err := r.dc.ContainerInspect(ctx, ctr.ID)
	exitCode := -1
	if err == nil && inspect.State != nil {
		exitCode = inspect.State.ExitCode
	}

	status := "completed"
	if exitCode != 0 {
		status = "failed"
	}

	r.auditEvent(ctx, exec.AgentID, exec.ID, CaseExitedRunning, "sync_status",
		map[string]interface{}{"status": "running"},
		map[string]interface{}{"status": status, "exit_code": exitCode},
	)

	_, _ = r.db.ExecContext(ctx,
		`UPDATE executions SET status=$1, exit_code=$2, completed_at=now(),
		 actual_state=$1, desired_state='stopped'
		 WHERE id=$3 AND status='running'`,
		status, exitCode, exec.ID)

	agentStatus := "stopped"
	if exitCode != 0 {
		agentStatus = "failed"
	}
	_ = r.updateAgentStatus(ctx, exec.AgentID, agentStatus)

	r.publishEvent("agent.execution.corrected", map[string]interface{}{
		"execution_id": exec.ID, "agent_id": exec.AgentID,
		"status": status, "exit_code": exitCode,
		"timestamp": time.Now().UTC(),
	})

	if exitCode != 0 {
		r.applyRestartPolicy(ctx, exec)
	}
}

func (r *Reconciler) enforceAgentDesiredState(ctx context.Context, agent AgentRecord, containerByAgent map[string]ContainerRecord) {
	// If desired=running but agent is failed/stopped and no container: trigger restart if policy allows
	if agent.DesiredState == "running" && agent.Status != "running" {
		if _, hasContainer := containerByAgent[agent.ID]; !hasContainer {
			if agent.RestartPolicy == "always" {
				r.logger.Info("enforcing desired state: triggering restart",
					"agent_id", agent.ID, "desired", agent.DesiredState, "actual", agent.Status)
				r.triggerAgentStart(ctx, agent.ID)
			}
		}
	}
}

// ── Restart policy ────────────────────────────────────────────────────────────

func (r *Reconciler) applyRestartPolicy(ctx context.Context, exec ExecutionRecord) {
	policy := exec.RestartPolicy
	if policy == "" {
		// Fall back to agent's restart policy
		var agentPolicy string
		_ = r.db.QueryRowContext(ctx,
			`SELECT COALESCE(restart_policy, 'never') FROM agents WHERE id=$1`, exec.AgentID,
		).Scan(&agentPolicy)
		policy = agentPolicy
	}

	if policy == "never" || policy == "" {
		return
	}
	if exec.Status == "completed" && policy != "always" {
		return
	}

	// Check max restart count
	maxRestarts := 5
	if exec.RestartCount >= maxRestarts {
		r.logger.Warn("max restart count reached, not restarting",
			"execution_id", exec.ID, "agent_id", exec.AgentID, "restart_count", exec.RestartCount)
		return
	}

	r.logger.Info("applying restart policy",
		"policy", policy, "agent_id", exec.AgentID, "restart_count", exec.RestartCount)

	// Increment agent restart count
	_, _ = r.db.ExecContext(ctx,
		`UPDATE agents SET restart_count = restart_count + 1 WHERE id=$1`, exec.AgentID)

	// Trigger start via executor
	go func() {
		time.Sleep(2 * time.Second) // backoff
		r.triggerAgentStart(ctx, exec.AgentID)
	}()

	r.publishEvent("agent.execution.restarted", map[string]interface{}{
		"execution_id": exec.ID, "agent_id": exec.AgentID,
		"policy": policy, "timestamp": time.Now().UTC(),
	})
}

// ── Database helpers ──────────────────────────────────────────────────────────

func (r *Reconciler) fetchContainers(ctx context.Context) ([]ContainerRecord, error) {
	f := filters.NewArgs()
	f.Add("label", "platform.managed=true")
	ctrs, err := r.dc.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("docker list: %w", err)
	}
	out := make([]ContainerRecord, 0, len(ctrs))
	for _, c := range ctrs {
		out = append(out, ContainerRecord{
			ID:      c.ID,
			AgentID: c.Labels["platform.agent_id"],
			State:   c.State,
			Status:  c.Status,
		})
	}
	return out, nil
}

func (r *Reconciler) fetchAgents(ctx context.Context) ([]AgentRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id::text, name, status,
		        COALESCE(desired_state,'stopped'), COALESCE(container_id,''),
		        COALESCE(node_id,''), COALESCE(restart_policy,'never'),
		        COALESCE(restart_count,0)
		 FROM agents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRecord
	for rows.Next() {
		var a AgentRecord
		if err := rows.Scan(&a.ID, &a.Name, &a.Status, &a.DesiredState,
			&a.ContainerID, &a.NodeID, &a.RestartPolicy, &a.RestartCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Reconciler) fetchActiveExecutions(ctx context.Context) ([]ExecutionRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id::text, agent_id::text, status,
		        COALESCE(desired_state,'running'), COALESCE(actual_state,'unknown'),
		        COALESCE(container_id,''), COALESCE(node_id,''),
		        COALESCE(restart_policy,'never'), COALESCE(restart_count,0),
		        started_at
		 FROM executions
		 WHERE status IN ('running','starting','stopping')
		    OR desired_state != actual_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExecutionRecord
	for rows.Next() {
		var e ExecutionRecord
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Status,
			&e.DesiredState, &e.ActualState, &e.ContainerID, &e.NodeID,
			&e.RestartPolicy, &e.RestartCount, &e.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Reconciler) fetchNodes(ctx context.Context) ([]NodeRecord, error) {
	// Check if table exists first
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='executor_nodes')`).Scan(&exists)
	if err != nil || !exists {
		return nil, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT node_id, status, last_heartbeat FROM executor_nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRecord
	for rows.Next() {
		var n NodeRecord
		if err := rows.Scan(&n.NodeID, &n.Status, &n.LastHeartbeat); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (r *Reconciler) markExecutionFailed(ctx context.Context, execID, reason string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE executions
		 SET status='failed', error=$1, completed_at=now(),
		     actual_state='failed', desired_state='stopped'
		 WHERE id=$2 AND status NOT IN ('completed','failed')`,
		reason, execID)
	return err
}

func (r *Reconciler) updateAgentStatus(ctx context.Context, agentID, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE agents SET status=$1, updated_at=now() WHERE id=$2`, status, agentID)
	return err
}

func (r *Reconciler) auditEvent(ctx context.Context, agentID, execID string, caseType ReconcileCase, action string, before, after map[string]interface{}) {
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)

	var execIDPtr interface{}
	if execID != "" {
		execIDPtr = execID
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO reconciliation_events
		 (agent_id, execution_id, case_type, action_taken, before_state, after_state)
		 VALUES (NULLIF($1,'')::uuid, $2::uuid, $3, $4, $5::jsonb, $6::jsonb)`,
		agentID, execIDPtr, string(caseType), action, string(beforeJSON), string(afterJSON))
	if err != nil {
		r.logger.Warn("failed to write reconciliation audit", "error", err)
	}
}

// ── Executor integration ──────────────────────────────────────────────────────

func (r *Reconciler) stopContainerViaExecutor(ctx context.Context, agentID, containerID string) {
	payload, _ := json.Marshal(map[string]string{
		"agent_id": agentID, "container_id": containerID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		r.cfg.ExecutorURL+"/containers/stop", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (r *Reconciler) triggerAgentStart(ctx context.Context, agentID string) {
	// Look up agent details
	var name, image, memLimit string
	var cpuLimit float64
	err := r.db.QueryRowContext(ctx,
		`SELECT name, image, cpu_limit, memory_limit FROM agents WHERE id=$1`, agentID,
	).Scan(&name, &image, &cpuLimit, &memLimit)
	if err != nil {
		r.logger.Error("failed to fetch agent for restart", "error", err, "agent_id", agentID)
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"agent_id":     agentID,
		"image":        image,
		"name":         fmt.Sprintf("agent-%s-%s", name, agentID[:8]),
		"cpu_limit":    cpuLimit,
		"memory_limit": memLimit,
		"env_vars":     map[string]string{},
		"labels":       map[string]string{},
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		r.cfg.ExecutorURL+"/containers/start", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err == nil {
		resp.Body.Close()
		r.logger.Info("triggered agent restart", "agent_id", agentID)
	} else {
		r.logger.Error("failed to trigger restart", "error", err, "agent_id", agentID)
	}
}

// ── NATS publisher ────────────────────────────────────────────────────────────

func (r *Reconciler) publishEvent(subject string, data map[string]interface{}) {
	if r.nc == nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	if err := r.nc.Publish(subject, payload); err != nil {
		r.logger.Warn("failed to publish NATS event", "subject", subject, "error", err)
	}
}

// ── Node heartbeat (this reconciler registers itself as a node) ───────────────

func (r *Reconciler) startHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	hostname, _ := os.Hostname()
	r.upsertNode(ctx, hostname)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.upsertNode(ctx, hostname)
		}
	}
}

func (r *Reconciler) upsertNode(ctx context.Context, hostname string) {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO executor_nodes (node_id, hostname, address, status, last_heartbeat)
		 VALUES ($1, $2, $3, 'healthy', now())
		 ON CONFLICT (node_id) DO UPDATE
		 SET last_heartbeat=now(), status='healthy', hostname=$2`,
		r.cfg.NodeID, hostname, r.cfg.ExecutorURL)
	if err != nil {
		// Table may not exist yet on first boot
		r.logger.Debug("heartbeat upsert failed (table may not exist yet)", "error", err)
	}
}

// ── HTTP API (health + metrics) ───────────────────────────────────────────────

func (r *Reconciler) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "reconciliation-service"})
	})

	router.GET("/reconciliation/events", func(c *gin.Context) {
		limit := 50
		rows, err := r.db.QueryContext(c.Request.Context(),
			`SELECT id, agent_id, execution_id, case_type, action_taken,
			        before_state, after_state, created_at
			 FROM reconciliation_events
			 ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var events []map[string]interface{}
		for rows.Next() {
			var id, caseType, action string
			var agentID, execID, beforeState, afterState *string
			var createdAt time.Time
			if err := rows.Scan(&id, &agentID, &execID, &caseType, &action,
				&beforeState, &afterState, &createdAt); err != nil {
				continue
			}
			events = append(events, map[string]interface{}{
				"id": id, "agent_id": agentID, "execution_id": execID,
				"case_type": caseType, "action_taken": action,
				"before_state": beforeState, "after_state": afterState,
				"created_at": createdAt,
			})
		}
		if events == nil {
			events = []map[string]interface{}{}
		}
		c.JSON(200, events)
	})

	router.GET("/nodes", func(c *gin.Context) {
		rows, err := r.db.QueryContext(c.Request.Context(),
			`SELECT node_id, hostname, address, status, current_load, last_heartbeat
			 FROM executor_nodes ORDER BY last_heartbeat DESC`)
		if err != nil {
			c.JSON(200, []interface{}{})
			return
		}
		defer rows.Close()
		var nodes []map[string]interface{}
		for rows.Next() {
			var nodeID, hostname, address, status string
			var load int
			var hb time.Time
			if err := rows.Scan(&nodeID, &hostname, &address, &status, &load, &hb); err != nil {
				continue
			}
			nodes = append(nodes, map[string]interface{}{
				"node_id": nodeID, "hostname": hostname, "address": address,
				"status": status, "current_load": load, "last_heartbeat": hb,
			})
		}
		if nodes == nil {
			nodes = []map[string]interface{}{}
		}
		c.JSON(200, nodes)
	})

	return router
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()

	logger.Info("starting reconciliation service", "node_id", cfg.NodeID)

	// ── Connect PostgreSQL ─────────────────────────────────────────────
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
		logger.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	defer db.Close()

	// ── Connect Docker ─────────────────────────────────────────────────
	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Error("failed to create docker client", "error", err)
		os.Exit(1)
	}
	defer dc.Close()

	ctx10, c10 := context.WithTimeout(context.Background(), 10*time.Second)
	info, err := dc.Info(ctx10)
	c10()
	if err != nil {
		logger.Error("cannot reach docker daemon", "error", err)
		os.Exit(1)
	}
	logger.Info("docker connected", "version", info.ServerVersion)

	// ── Connect NATS ───────────────────────────────────────────────────
	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL,
			natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(2*time.Second),
			natsgo.Name("reconciliation-service"),
		)
		if err == nil {
			break
		}
		logger.Info("waiting for nats", "attempt", i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Error("failed to connect to nats", "error", err)
		os.Exit(1)
	}
	defer nc.Close()
	logger.Info("nats connected")

	// ── Start services ─────────────────────────────────────────────────
	rec := NewReconciler(cfg, db, dc, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rec.Run(ctx)
	go rec.startHeartbeat(ctx)

	// ── HTTP server ────────────────────────────────────────────────────
	router := rec.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("http server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
		}
	}()

	// ── Graceful shutdown ──────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	logger.Info("shutting down")
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}