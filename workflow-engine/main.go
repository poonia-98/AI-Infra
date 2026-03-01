package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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

// ── Config ─────────────────────────────────────────────────────────────────

type Config struct {
	DatabaseURL    string
	NatsURL        string
	BackendURL     string
	ControlPlaneAPIKey string
	WebhookAllowedDomains string
	Port           string
	WorkerCount    int
	PollInterval   time.Duration
}

func loadConfig() Config {
	poll, _ := time.ParseDuration(getEnv("POLL_INTERVAL", "2s"))
	workers := 16
	return Config{
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:      getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:   getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		ControlPlaneAPIKey: getEnv("CONTROL_PLANE_API_KEY", ""),
		WebhookAllowedDomains: getEnv("WEBHOOK_ALLOWED_DOMAINS", ""),
		Port:         getEnv("PORT", "8092"),
		WorkerCount:  workers,
		PollInterval: poll,
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────

var (
	workflowsStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_workflows_started_total",
	})
	workflowsCompleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_workflows_completed_total",
	}, []string{"status"})
	workflowDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "platform_workflow_duration_seconds",
		Buckets: []float64{1, 5, 10, 30, 60, 300, 600, 3600},
	})
	activeWorkflows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_workflows_active",
	})
)

func init() {
	prometheus.MustRegister(workflowsStarted, workflowsCompleted, workflowDuration, activeWorkflows)
}

// ── Domain ────────────────────────────────────────────────────────────────

type DAGNode struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"` // agent|condition|transform|wait|webhook|parallel_gate
	AgentID      string                 `json:"agent_id,omitempty"`
	Config       map[string]interface{} `json:"config,omitempty"`
	InputMapping map[string]string      `json:"input_mapping,omitempty"`  // context_key -> node_input_key
	OutputMapping map[string]string     `json:"output_mapping,omitempty"` // node_output_key -> context_key
	TimeoutSec   int                    `json:"timeout_seconds,omitempty"`
	Retries      int                    `json:"retries,omitempty"`
	Condition    string                 `json:"condition,omitempty"`     // for condition nodes: JSONPath expression
	ConditionTrue  string               `json:"condition_true,omitempty"`  // next node if true
	ConditionFalse string               `json:"condition_false,omitempty"` // next node if false
	WaitSeconds  int                    `json:"wait_seconds,omitempty"`  // for wait nodes
}

type DAGEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Label string `json:"label,omitempty"` // true|false|default
}

type DAG struct {
	Nodes []DAGNode `json:"nodes"`
	Edges []DAGEdge `json:"edges"`
}

type Workflow struct {
	ID             string
	OrganisationID string
	Name           string
	DAG            DAG
	TriggerType    string
	Status         string
	MaxRetries     int
	TimeoutSeconds int
}

type WorkflowExecution struct {
	ID             string
	WorkflowID     string
	OrganisationID string
	Status         string
	Input          map[string]interface{}
	Output         map[string]interface{}
	Context        map[string]interface{} // shared mutable state
}

type NodeExecution struct {
	ID              string
	WorkflowExecID  string
	NodeID          string
	NodeType        string
	AgentID         string
	Status          string
	Input           map[string]interface{}
	Output          map[string]interface{}
	Attempt         int
}

// ── Engine ────────────────────────────────────────────────────────────────

type WorkflowEngine struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
	jobs   chan string // workflow execution IDs
	wg     sync.WaitGroup
}

func NewWorkflowEngine(cfg Config, db *sql.DB, nc *natsgo.Conn) *WorkflowEngine {
	return &WorkflowEngine{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 30 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		jobs:   make(chan string, 256),
	}
}

// ── Execution dispatch ────────────────────────────────────────────────────

func (e *WorkflowEngine) StartWorkers(ctx context.Context) {
	for i := 0; i < e.cfg.WorkerCount; i++ {
		e.wg.Add(1)
		go func(id int) {
			defer e.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case execID := <-e.jobs:
					e.executeWorkflow(ctx, execID)
				}
			}
		}(i)
	}
	e.logger.Info("workflow workers started", "count", e.cfg.WorkerCount)
}

func (e *WorkflowEngine) RunPoller(ctx context.Context) {
	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.pollPendingExecutions(ctx)
		}
	}
}

func (e *WorkflowEngine) pollPendingExecutions(ctx context.Context) {
	rows, err := e.db.QueryContext(ctx, `
		UPDATE workflow_executions
		SET status='running', started_at=NOW(), updated_at=NOW()
		WHERE id IN (
			SELECT id FROM workflow_executions
			WHERE status='pending'
			ORDER BY created_at ASC
			LIMIT 32
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			select {
			case e.jobs <- id:
				workflowsStarted.Inc()
				activeWorkflows.Inc()
			default:
			}
		}
	}
}

// ── Core execution loop ───────────────────────────────────────────────────

func (e *WorkflowEngine) executeWorkflow(ctx context.Context, execID string) {
	start := time.Now()
	defer activeWorkflows.Dec()

	exec, wf, err := e.loadExecution(ctx, execID)
	if err != nil {
		e.logger.Error("load execution failed", "exec_id", execID, "error", err)
		e.failExecution(execID, "load failed: "+err.Error())
		return
	}

	e.publishEvent("workflow.started", map[string]interface{}{
		"workflow_id": wf.ID, "execution_id": execID, "name": wf.Name,
	})

	// Build adjacency map
	adj := e.buildAdjacency(wf.DAG)
	startNodes := e.findStartNodes(wf.DAG)

	if err := e.runDAG(ctx, exec, wf, startNodes, adj); err != nil {
		e.failExecution(execID, err.Error())
		workflowsCompleted.With(prometheus.Labels{"status": "failed"}).Inc()
		e.publishEvent("workflow.failed", map[string]interface{}{
			"workflow_id": wf.ID, "execution_id": execID, "error": err.Error(),
		})
		return
	}

	dur := time.Since(start)
	e.completeExecution(execID, exec.Context)
	workflowsCompleted.With(prometheus.Labels{"status": "completed"}).Inc()
	workflowDuration.Observe(dur.Seconds())
	e.publishEvent("workflow.completed", map[string]interface{}{
		"workflow_id": wf.ID, "execution_id": execID, "duration_ms": dur.Milliseconds(),
	})
}

func (e *WorkflowEngine) runDAG(ctx context.Context, exec *WorkflowExecution, wf *Workflow,
	toRun []string, adj map[string][]DAGEdge) error {

	if len(toRun) == 0 {
		return nil
	}

	// Execute current level nodes in parallel where possible
	type result struct {
		nodeID string
		output map[string]interface{}
		next   []string
		err    error
	}

	results := make(chan result, len(toRun))
	var wg sync.WaitGroup

	for _, nodeID := range toRun {
		node := e.findNode(wf.DAG.Nodes, nodeID)
		if node == nil {
			continue
		}
		wg.Add(1)
		go func(n DAGNode) {
			defer wg.Done()
			out, nextNodes, err := e.executeNode(ctx, exec, wf, n, adj)
			results <- result{nodeID: n.ID, output: out, next: nextNodes, err: err}
		}(*node)
	}

	wg.Wait()
	close(results)

	// Collect and merge outputs into context
	var nextLevel []string
	seen := map[string]bool{}
	for r := range results {
		if r.err != nil {
			return fmt.Errorf("node %s failed: %w", r.nodeID, r.err)
		}
		// Apply output mapping to shared context
		if node := e.findNode(wf.DAG.Nodes, r.nodeID); node != nil {
			for outKey, ctxKey := range node.OutputMapping {
				if val, ok := r.output[outKey]; ok {
					exec.Context[ctxKey] = val
				}
			}
		}
		for _, next := range r.next {
			if !seen[next] {
				seen[next] = true
				nextLevel = append(nextLevel, next)
			}
		}
	}

	// Persist context
	e.saveContext(exec.ID, exec.Context)

	// Recurse into next level
	return e.runDAG(ctx, exec, wf, nextLevel, adj)
}

func (e *WorkflowEngine) executeNode(ctx context.Context, exec *WorkflowExecution,
	wf *Workflow, node DAGNode, adj map[string][]DAGEdge) (map[string]interface{}, []string, error) {

	nodeExecID := e.createNodeExecution(exec.ID, node)
	defer func() {
		// best-effort node exec cleanup on panic
	}()

	timeout := 300 * time.Second
	if node.TimeoutSec > 0 {
		timeout = time.Duration(node.TimeoutSec) * time.Second
	}
	nodeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var output map[string]interface{}
	var err error

	switch node.Type {
	case "agent":
		output, err = e.executeAgentNode(nodeCtx, exec, node)

	case "condition":
		result, condErr := e.evaluateCondition(node.Condition, exec.Context)
		if condErr != nil {
			err = condErr
		} else {
			output = map[string]interface{}{"result": result}
			if result {
				if node.ConditionTrue != "" {
					e.completeNodeExecution(nodeExecID, output, "")
					return output, []string{node.ConditionTrue}, nil
				}
			} else {
				if node.ConditionFalse != "" {
					e.completeNodeExecution(nodeExecID, output, "")
					return output, []string{node.ConditionFalse}, nil
				}
			}
		}

	case "transform":
		output = e.executeTransform(node, exec.Context)

	case "wait":
		if node.WaitSeconds > 0 {
			select {
			case <-nodeCtx.Done():
				err = nodeCtx.Err()
			case <-time.After(time.Duration(node.WaitSeconds) * time.Second):
			}
		}
		output = map[string]interface{}{"waited_seconds": node.WaitSeconds}

	case "webhook":
		output, err = e.callWebhook(nodeCtx, node, exec.Context)

	default:
		output = map[string]interface{}{}
	}

	if err != nil {
		e.failNodeExecution(nodeExecID, err.Error())
		return nil, nil, err
	}

	e.completeNodeExecution(nodeExecID, output, "")

	// Determine next nodes from edge list
	var next []string
	for _, edge := range adj[node.ID] {
		next = append(next, edge.To)
	}
	return output, next, nil
}

func (e *WorkflowEngine) executeAgentNode(ctx context.Context, exec *WorkflowExecution,
	node DAGNode) (map[string]interface{}, error) {

	// Build input by mapping context keys
	input := make(map[string]interface{})
	for ctxKey, nodeKey := range node.InputMapping {
		if val, ok := exec.Context[ctxKey]; ok {
			input[nodeKey] = val
		}
	}
	// Merge node-level config
	for k, v := range node.Config {
		if _, exists := input[k]; !exists {
			input[k] = v
		}
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"input":       input,
		"workflow_id": exec.WorkflowID,
		"exec_id":     exec.ID,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		e.cfg.BackendURL+"/api/v1/agents/"+node.AgentID+"/execute",
		bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workflow-Exec", exec.ID)
	if e.cfg.ControlPlaneAPIKey != "" {
		req.Header.Set("X-Service-API-Key", e.cfg.ControlPlaneAPIKey)
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("agent returned %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

func (e *WorkflowEngine) evaluateCondition(condition string, context map[string]interface{}) (bool, error) {
	// Simple key=value evaluator; extend with JSONPath or expression engine
	if condition == "" {
		return true, nil
	}
	// Support: "key==value", "key>number", "key!=value"
	for _, op := range []string{"==", "!=", ">=", "<=", ">", "<"} {
		parts := splitOn(condition, op)
		if len(parts) == 2 {
			key := trim(parts[0])
			expected := trim(parts[1])
			val, ok := context[key]
			if !ok {
				return false, nil
			}
			actual := fmt.Sprintf("%v", val)
			switch op {
			case "==":
				return actual == expected, nil
			case "!=":
				return actual != expected, nil
			}
		}
	}
	return false, nil
}

func (e *WorkflowEngine) executeTransform(node DAGNode, ctx map[string]interface{}) map[string]interface{} {
	output := make(map[string]interface{})
	if mappings, ok := node.Config["mappings"].(map[string]interface{}); ok {
		for outKey, srcKey := range mappings {
			if s, ok := srcKey.(string); ok {
				if val, exists := ctx[s]; exists {
					output[outKey] = val
				}
			}
		}
	}
	return output
}

func (e *WorkflowEngine) callWebhook(ctx context.Context, node DAGNode, wfCtx map[string]interface{}) (map[string]interface{}, error) {
	url, _ := node.Config["url"].(string)
	if url == "" {
		return map[string]interface{}{}, nil
	}
	if err := validateWebhookURL(url, e.cfg.WebhookAllowedDomains); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(wfCtx)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// ── DAG helpers ───────────────────────────────────────────────────────────

func (e *WorkflowEngine) buildAdjacency(dag DAG) map[string][]DAGEdge {
	adj := make(map[string][]DAGEdge)
	for _, edge := range dag.Edges {
		adj[edge.From] = append(adj[edge.From], edge)
	}
	return adj
}

func (e *WorkflowEngine) findStartNodes(dag DAG) []string {
	hasIncoming := map[string]bool{}
	for _, edge := range dag.Edges {
		hasIncoming[edge.To] = true
	}
	var starts []string
	for _, node := range dag.Nodes {
		if !hasIncoming[node.ID] {
			starts = append(starts, node.ID)
		}
	}
	return starts
}

func (e *WorkflowEngine) findNode(nodes []DAGNode, id string) *DAGNode {
	for i := range nodes {
		if nodes[i].ID == id {
			return &nodes[i]
		}
	}
	return nil
}

// ── DB helpers ────────────────────────────────────────────────────────────

func (e *WorkflowEngine) loadExecution(ctx context.Context, execID string) (*WorkflowExecution, *Workflow, error) {
	row := e.db.QueryRowContext(ctx, `
		SELECT we.id, we.workflow_id, COALESCE(we.organisation_id::text,''),
		       we.status, we.input, we.output, we.context,
		       w.id, COALESCE(w.organisation_id::text,''), w.name, w.dag,
		       w.max_retries, w.timeout_seconds
		FROM workflow_executions we
		JOIN workflows w ON w.id = we.workflow_id
		WHERE we.id = $1`, execID)

	var exec WorkflowExecution
	var wf Workflow
	var inputJSON, outputJSON, ctxJSON, dagJSON []byte
	err := row.Scan(
		&exec.ID, &exec.WorkflowID, &exec.OrganisationID,
		&exec.Status, &inputJSON, &outputJSON, &ctxJSON,
		&wf.ID, &wf.OrganisationID, &wf.Name, &dagJSON,
		&wf.MaxRetries, &wf.TimeoutSeconds,
	)
	if err != nil {
		return nil, nil, err
	}
	json.Unmarshal(inputJSON, &exec.Input)
	json.Unmarshal(outputJSON, &exec.Output)
	json.Unmarshal(ctxJSON, &exec.Context)
	json.Unmarshal(dagJSON, &wf.DAG)
	if exec.Context == nil {
		exec.Context = make(map[string]interface{})
	}
	// Seed context with input
	for k, v := range exec.Input {
		if _, exists := exec.Context[k]; !exists {
			exec.Context[k] = v
		}
	}
	return &exec, &wf, nil
}

func (e *WorkflowEngine) createNodeExecution(execID string, node DAGNode) string {
	var id string
	e.db.QueryRow(`
		INSERT INTO workflow_node_executions
		  (workflow_exec_id, node_id, node_type, agent_id, status)
		VALUES ($1, $2, $3, NULLIF($4,'')::uuid, 'running')
		RETURNING id`,
		execID, node.ID, node.Type, node.AgentID).Scan(&id)
	return id
}

func (e *WorkflowEngine) completeNodeExecution(id string, output map[string]interface{}, errMsg string) {
	outJSON, _ := json.Marshal(output)
	e.db.Exec(`
		UPDATE workflow_node_executions
		SET status='completed', output=$1::jsonb, completed_at=NOW(),
		    duration_ms=EXTRACT(EPOCH FROM (NOW()-started_at))*1000
		WHERE id=$2`, string(outJSON), id)
}

func (e *WorkflowEngine) failNodeExecution(id, errMsg string) {
	e.db.Exec(`
		UPDATE workflow_node_executions
		SET status='failed', error=$1, completed_at=NOW()
		WHERE id=$2`, errMsg, id)
}

func (e *WorkflowEngine) saveContext(execID string, ctx map[string]interface{}) {
	ctxJSON, _ := json.Marshal(ctx)
	e.db.Exec(`UPDATE workflow_executions SET context=$1::jsonb, updated_at=NOW() WHERE id=$2`,
		string(ctxJSON), execID)
}

func (e *WorkflowEngine) completeExecution(execID string, output map[string]interface{}) {
	outJSON, _ := json.Marshal(output)
	e.db.Exec(`
		UPDATE workflow_executions
		SET status='completed', output=$1::jsonb, completed_at=NOW(),
		    duration_ms=EXTRACT(EPOCH FROM (NOW()-started_at))*1000,
		    updated_at=NOW()
		WHERE id=$2`, string(outJSON), execID)
}

func (e *WorkflowEngine) failExecution(execID, errMsg string) {
	e.db.Exec(`
		UPDATE workflow_executions
		SET status='failed', error=$1, completed_at=NOW(), updated_at=NOW()
		WHERE id=$2`, errMsg, execID)
}

// ── NATS event publisher ──────────────────────────────────────────────────

func (e *WorkflowEngine) publishEvent(subject string, data map[string]interface{}) {
	if e.nc == nil {
		return
	}
	data["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	payload, _ := json.Marshal(data)
	_ = e.nc.Publish("workflows.events", payload)
	_ = e.nc.Publish(subject, payload)
}

// ── NATS subscription (trigger workflow from events) ──────────────────────

func (e *WorkflowEngine) subscribeToTriggers(ctx context.Context) {
	// Listen for event-driven workflow triggers
	e.nc.Subscribe("workflows.trigger", func(msg *natsgo.Msg) {
		var trigger struct {
			WorkflowID string                 `json:"workflow_id"`
			Input      map[string]interface{} `json:"input"`
			TriggeredBy string                `json:"triggered_by"`
		}
		if err := json.Unmarshal(msg.Data, &trigger); err != nil {
			return
		}
		inputJSON, _ := json.Marshal(trigger.Input)
		var execID string
		e.db.QueryRow(`
			INSERT INTO workflow_executions (workflow_id, status, input, triggered_by)
			SELECT $1::uuid, 'pending', $2::jsonb, $3
			FROM workflows WHERE id=$1::uuid AND status='active'
			RETURNING id`,
			trigger.WorkflowID, string(inputJSON), trigger.TriggeredBy).Scan(&execID)
		if execID != "" {
			e.logger.Info("workflow triggered via NATS", "workflow_id", trigger.WorkflowID, "exec_id", execID)
		}
	})

	e.nc.Subscribe("agents.events", func(msg *natsgo.Msg) {
		var event struct {
			EventType string `json:"event_type"`
			AgentID   string `json:"agent_id"`
			ExecID    string `json:"execution_id"`
			Output    map[string]interface{} `json:"output"`
		}
		json.Unmarshal(msg.Data, &event)
		// When an agent execution completes, resume any waiting workflow node
		if event.EventType == "agent.completed" || event.EventType == "execution.completed" {
			e.resumeWaitingNode(ctx, event.ExecID, event.Output)
		}
	})
}

func (e *WorkflowEngine) resumeWaitingNode(ctx context.Context, executionID string, output map[string]interface{}) {
	var nodeExecID, wfExecID string
	e.db.QueryRow(`
		SELECT id, workflow_exec_id FROM workflow_node_executions
		WHERE execution_id=$1::uuid AND status='running'`,
		executionID).Scan(&nodeExecID, &wfExecID)
	if nodeExecID == "" {
		return
	}
	e.completeNodeExecution(nodeExecID, output, "")
}

// ── HTTP API ─────────────────────────────────────────────────────────────

func (e *WorkflowEngine) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	requireServiceAuth := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			got := c.GetHeader("X-Service-API-Key")
			if e.cfg.ControlPlaneAPIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(e.cfg.ControlPlaneAPIKey)) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "workflow-engine"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := e.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	protected := r.Group("/")
	protected.Use(requireServiceAuth())

	// Trigger a workflow execution
	protected.POST("/workflows/:id/execute", func(c *gin.Context) {
		var body struct {
			Input       map[string]interface{} `json:"input"`
			TriggeredBy string                 `json:"triggered_by"`
		}
		c.ShouldBindJSON(&body)
		inputJSON, _ := json.Marshal(body.Input)
		var execID string
		err := e.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO workflow_executions (workflow_id, status, input, triggered_by)
			SELECT $1::uuid, 'pending', $2::jsonb, $3
			FROM workflows WHERE id=$1::uuid AND status='active'
			RETURNING id`,
			c.Param("id"), string(inputJSON), body.TriggeredBy).Scan(&execID)
		if err != nil || execID == "" {
			c.JSON(404, gin.H{"error": "workflow not found or not active"})
			return
		}
		c.JSON(202, gin.H{"execution_id": execID, "status": "pending"})
	})

	// Get workflow execution status
	protected.GET("/workflows/executions/:id", func(c *gin.Context) {
		row := e.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text, workflow_id::text, status, output, error,
			       started_at, completed_at, duration_ms
			FROM workflow_executions WHERE id=$1::uuid`, c.Param("id"))
		var id, wfID, status, errMsg string
		var outputJSON []byte
		var startedAt, completedAt *time.Time
		var durationMs *int64
		if err := row.Scan(&id, &wfID, &status, &outputJSON, &errMsg, &startedAt, &completedAt, &durationMs); err != nil {
			c.JSON(404, gin.H{"error": "not found"})
			return
		}
		var output interface{}
		json.Unmarshal(outputJSON, &output)
		c.JSON(200, gin.H{
			"id": id, "workflow_id": wfID, "status": status,
			"output": output, "error": errMsg,
			"started_at": startedAt, "completed_at": completedAt,
			"duration_ms": durationMs,
		})
	})

	// List node executions for a workflow execution
	protected.GET("/workflows/executions/:id/nodes", func(c *gin.Context) {
		rows, err := e.db.QueryContext(c.Request.Context(), `
			SELECT id::text, node_id, node_type, status, error,
			       started_at, completed_at, duration_ms, attempt
			FROM workflow_node_executions WHERE workflow_exec_id=$1::uuid
			ORDER BY started_at`, c.Param("id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var nodes []map[string]interface{}
		for rows.Next() {
			var id, nodeID, nodeType, status, errMsg string
			var startedAt, completedAt *time.Time
			var durationMs *int64
			var attempt int
			rows.Scan(&id, &nodeID, &nodeType, &status, &errMsg, &startedAt, &completedAt, &durationMs, &attempt)
			nodes = append(nodes, map[string]interface{}{
				"id": id, "node_id": nodeID, "node_type": nodeType,
				"status": status, "error": errMsg,
				"started_at": startedAt, "completed_at": completedAt,
				"duration_ms": durationMs, "attempt": attempt,
			})
		}
		if nodes == nil {
			nodes = []map[string]interface{}{}
		}
		c.JSON(200, nodes)
	})

	return r
}

// ── Main ─────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting workflow-engine", "port", cfg.Port)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil {
			if db.PingContext(context.Background()) == nil {
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
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(2*time.Second), natsgo.Name("workflow-engine"))
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

	engine := NewWorkflowEngine(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine.subscribeToTriggers(ctx)
	engine.StartWorkers(ctx)
	go engine.RunPoller(ctx)

	router := engine.setupRouter()
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
	engine.wg.Wait()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
	logger.Info("workflow-engine stopped")
}

// ── Util ──────────────────────────────────────────────────────────────────

func splitOn(s, sep string) []string {
	idx := 0
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			idx = i
			return []string{s[:idx], s[idx+len(sep):]}
		}
	}
	return []string{s}
}

func trim(s string) string {
	result := []byte{}
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			result = append(result, s[i])
		}
	}
	return string(result)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func validateWebhookURL(raw string, allowedDomainsCSV string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("invalid webhook url")
	}
	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, item := range strings.Split(allowedDomainsCSV, ",") {
		domain := strings.ToLower(strings.TrimSpace(item))
		if domain == "" {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("webhook domain not allowed")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("webhook destination blocked")
		}
	}
	return nil
}
