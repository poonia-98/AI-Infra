package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type NodeExecutor interface {
	Execute(ctx context.Context, workflowID string, node Node, state map[string]interface{}) (map[string]interface{}, error)
}

type HTTPNodeExecutor struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPNodeExecutor(baseURL string, timeout time.Duration) *HTTPNodeExecutor {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &HTTPNodeExecutor{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: timeout},
	}
}

func (e *HTTPNodeExecutor) Execute(ctx context.Context, workflowID string, node Node, state map[string]interface{}) (map[string]interface{}, error) {
	switch node.Type {
	case "agent":
		return e.executeAgent(ctx, workflowID, node, state)
	case "wait":
		seconds := node.TimeoutSeconds
		if seconds <= 0 {
			seconds = 1
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(seconds) * time.Second):
			return map[string]interface{}{"waited": seconds}, nil
		}
	case "condition":
		return map[string]interface{}{"condition": evalExpression(node.Condition, state)}, nil
	case "transform":
		out := map[string]interface{}{}
		for k, v := range node.Input {
			if s, ok := v.(string); ok {
				if stateValue, exists := state[s]; exists {
					out[k] = stateValue
					continue
				}
			}
			out[k] = v
		}
		return out, nil
	default:
		return map[string]interface{}{}, nil
	}
}

func (e *HTTPNodeExecutor) executeAgent(ctx context.Context, workflowID string, node Node, state map[string]interface{}) (map[string]interface{}, error) {
	if strings.TrimSpace(node.AgentID) == "" {
		return nil, fmt.Errorf("agent node %s missing agent_id", node.ID)
	}
	payload := map[string]interface{}{
		"workflow_id": workflowID,
		"node_id":     node.ID,
		"input":       mergeInput(node.Input, state),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/api/v1/agents/"+node.AgentID+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv("CONTROL_PLANE_API_KEY"); apiKey != "" {
		req.Header.Set("X-Service-API-Key", apiKey)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("agent execution failed with status %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func mergeInput(nodeInput map[string]interface{}, state map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range state {
		out[k] = v
	}
	for k, v := range nodeInput {
		out[k] = v
	}
	return out
}
