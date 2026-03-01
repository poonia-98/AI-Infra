package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

type WorkflowDefinition struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	DAG            DAG         `json:"dag"`
	RetryPolicy    RetryPolicy `json:"retry_policy"`
	TimeoutSeconds int         `json:"timeout_seconds"`
}

type ExecutionResult struct {
	ExecutionID string                 `json:"execution_id"`
	WorkflowID  string                 `json:"workflow_id"`
	Status      string                 `json:"status"`
	Output      map[string]interface{} `json:"output"`
	Error       string                 `json:"error,omitempty"`
	StartedAt   time.Time              `json:"started_at"`
	FinishedAt  time.Time              `json:"finished_at"`
}

type Engine struct {
	executor      NodeExecutor
	queue         *ExecutionQueue
	logger        *slog.Logger
	workflows     map[string]WorkflowDefinition
	eventTriggers map[string][]string
	mu            sync.RWMutex
	onComplete    func(ExecutionResult)
}

func NewEngine(executor NodeExecutor, queueSize int, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		executor:      executor,
		queue:         NewExecutionQueue(queueSize),
		logger:        logger,
		workflows:     map[string]WorkflowDefinition{},
		eventTriggers: map[string][]string{},
	}
}

func (e *Engine) SetCompletionHandler(fn func(ExecutionResult)) {
	e.onComplete = fn
}

func (e *Engine) RegisterWorkflow(def WorkflowDefinition) error {
	if def.ID == "" {
		return errors.New("workflow id is required")
	}
	if def.TimeoutSeconds <= 0 {
		def.TimeoutSeconds = 3600
	}
	if def.RetryPolicy.MaxAttempts <= 0 {
		def.RetryPolicy.MaxAttempts = 1
	}
	if err := def.DAG.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	e.workflows[def.ID] = def
	e.mu.Unlock()
	return nil
}

func (e *Engine) RegisterEventTrigger(eventName, workflowID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.eventTriggers[eventName] = append(e.eventTriggers[eventName], workflowID)
}

func (e *Engine) Trigger(ctx context.Context, workflowID string, input map[string]interface{}, triggerType string) (string, error) {
	executionID := fmt.Sprintf("%s-%d-%d", workflowID, time.Now().UnixNano(), rand.Intn(10000))
	err := e.queue.Enqueue(ctx, ExecutionJob{
		ExecutionID: executionID,
		WorkflowID:  workflowID,
		Input:       input,
		TriggerType: triggerType,
	})
	if err != nil {
		return "", err
	}
	return executionID, nil
}

func (e *Engine) TriggerEvent(ctx context.Context, eventName string, payload map[string]interface{}) ([]string, error) {
	e.mu.RLock()
	workflowIDs := append([]string{}, e.eventTriggers[eventName]...)
	e.mu.RUnlock()
	if len(workflowIDs) == 0 {
		return []string{}, nil
	}
	out := make([]string, 0, len(workflowIDs))
	for _, wfID := range workflowIDs {
		executionID, err := e.Trigger(ctx, wfID, payload, "event:"+eventName)
		if err != nil {
			return out, err
		}
		out = append(out, executionID)
	}
	return out, nil
}

func (e *Engine) Run(ctx context.Context, workers int) {
	if workers <= 0 {
		workers = 4
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				job, err := e.queue.Dequeue(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					time.Sleep(100 * time.Millisecond)
					continue
				}
				result := e.executeJob(ctx, job)
				if e.onComplete != nil {
					e.onComplete(result)
				}
				e.logger.Info("workflow execution finished",
					"worker", workerID,
					"workflow_id", job.WorkflowID,
					"execution_id", job.ExecutionID,
					"status", result.Status)
			}
		}(i + 1)
	}
	<-ctx.Done()
	e.queue.Close()
	wg.Wait()
}

func (e *Engine) executeJob(ctx context.Context, job ExecutionJob) ExecutionResult {
	result := ExecutionResult{
		ExecutionID: job.ExecutionID,
		WorkflowID:  job.WorkflowID,
		Status:      "failed",
		Output:      map[string]interface{}{},
		StartedAt:   time.Now().UTC(),
	}

	e.mu.RLock()
	workflow, ok := e.workflows[job.WorkflowID]
	e.mu.RUnlock()
	if !ok {
		result.Error = "workflow not found"
		result.FinishedAt = time.Now().UTC()
		return result
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(workflow.TimeoutSeconds)*time.Second)
	defer cancel()

	state := map[string]interface{}{}
	for k, v := range job.Input {
		state[k] = v
	}
	done := map[string]bool{}
	current := workflow.DAG.StartNodes()

	for len(current) > 0 {
		if runCtx.Err() != nil {
			result.Error = runCtx.Err().Error()
			result.FinishedAt = time.Now().UTC()
			return result
		}

		next := make(chan []string, len(current))
		errs := make(chan error, len(current))
		var stateMu sync.Mutex
		var wg sync.WaitGroup

		for _, nodeID := range current {
			if done[nodeID] {
				continue
			}
			node, exists := workflow.DAG.NodeByID(nodeID)
			if !exists {
				errs <- fmt.Errorf("node %s not found", nodeID)
				continue
			}
			done[nodeID] = true
			wg.Add(1)
			go func(n Node) {
				defer wg.Done()
				out, err := e.executeNode(runCtx, workflow, n, state)
				if err != nil {
					errs <- err
					return
				}
				stateMu.Lock()
				state[n.ID] = out
				for k, v := range out {
					state[n.ID+"."+k] = v
				}
				stateMu.Unlock()
				next <- workflow.DAG.Next(n, state, out)
			}(node)
		}

		wg.Wait()
		close(next)
		close(errs)

		for err := range errs {
			if err != nil {
				result.Error = err.Error()
				result.FinishedAt = time.Now().UTC()
				return result
			}
		}

		merged := []string{}
		for level := range next {
			merged = append(merged, level...)
		}
		current = dedupe(merged)
	}

	result.Status = "completed"
	result.Output = state
	result.FinishedAt = time.Now().UTC()
	return result
}

func (e *Engine) executeNode(ctx context.Context, workflow WorkflowDefinition, node Node, state map[string]interface{}) (map[string]interface{}, error) {
	maxAttempts := workflow.RetryPolicy.MaxAttempts
	backoff := parseBackoff(workflow.RetryPolicy.Backoff)
	if node.RetryPolicy != nil {
		if node.RetryPolicy.MaxAttempts > 0 {
			maxAttempts = node.RetryPolicy.MaxAttempts
		}
		if node.RetryPolicy.Backoff != "" {
			backoff = parseBackoff(node.RetryPolicy.Backoff)
		}
	}
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		nodeCtx := ctx
		if node.TimeoutSeconds > 0 {
			var cancel context.CancelFunc
			nodeCtx, cancel = context.WithTimeout(ctx, time.Duration(node.TimeoutSeconds)*time.Second)
			defer cancel()
		}
		out, err := e.executor.Execute(nodeCtx, workflow.ID, node, copyState(state))
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff * time.Duration(attempt)):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("node execution failed")
	}
	return nil, fmt.Errorf("node %s failed: %w", node.ID, lastErr)
}

func copyState(src map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func parseBackoff(v string) time.Duration {
	if v == "" {
		return 250 * time.Millisecond
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Millisecond
	}
	return 250 * time.Millisecond
}
