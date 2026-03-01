package internal

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type RetryPolicy struct {
	MaxAttempts int    `json:"max_attempts"`
	Backoff     string `json:"backoff"`
}

type Node struct {
	ID             string                 `json:"id"`
	Type           string                 `json:"type"`
	AgentID        string                 `json:"agent_id,omitempty"`
	Input          map[string]interface{} `json:"input,omitempty"`
	Condition      string                 `json:"condition,omitempty"`
	TimeoutSeconds int                    `json:"timeout_seconds,omitempty"`
	RetryPolicy    *RetryPolicy           `json:"retry_policy,omitempty"`
}

type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

type DAG struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

func (d DAG) Validate() error {
	if len(d.Nodes) == 0 {
		return errors.New("dag must contain nodes")
	}
	index := make(map[string]Node, len(d.Nodes))
	for _, n := range d.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			return errors.New("node id is required")
		}
		if _, exists := index[n.ID]; exists {
			return fmt.Errorf("duplicate node id: %s", n.ID)
		}
		index[n.ID] = n
	}
	for _, e := range d.Edges {
		if _, ok := index[e.From]; !ok {
			return fmt.Errorf("edge source node not found: %s", e.From)
		}
		if _, ok := index[e.To]; !ok {
			return fmt.Errorf("edge target node not found: %s", e.To)
		}
	}
	return nil
}

func (d DAG) StartNodes() []string {
	incoming := map[string]int{}
	for _, n := range d.Nodes {
		incoming[n.ID] = 0
	}
	for _, e := range d.Edges {
		incoming[e.To]++
	}
	var starts []string
	for _, n := range d.Nodes {
		if incoming[n.ID] == 0 {
			starts = append(starts, n.ID)
		}
	}
	return starts
}

func (d DAG) NodeByID(id string) (Node, bool) {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (d DAG) Next(node Node, state map[string]interface{}, result map[string]interface{}) []string {
	var out []string
	if node.Type == "condition" {
		cond := conditionResult(node, state, result)
		for _, e := range d.Edges {
			if e.From != node.ID {
				continue
			}
			label := strings.ToLower(strings.TrimSpace(e.Condition))
			if cond && (label == "true" || label == "yes") {
				out = append(out, e.To)
			}
			if !cond && (label == "false" || label == "no") {
				out = append(out, e.To)
			}
			if label == "" || label == "default" {
				out = append(out, e.To)
			}
		}
		return dedupe(out)
	}
	for _, e := range d.Edges {
		if e.From == node.ID {
			out = append(out, e.To)
		}
	}
	return dedupe(out)
}

func conditionResult(node Node, state map[string]interface{}, result map[string]interface{}) bool {
	if v, ok := result["condition"].(bool); ok {
		return v
	}
	return evalExpression(node.Condition, state)
}

func evalExpression(expr string, state map[string]interface{}) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true
	}
	ops := []string{"==", "!=", ">=", "<=", ">", "<"}
	for _, op := range ops {
		parts := strings.SplitN(expr, op, 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		raw, ok := state[left]
		if !ok {
			return false
		}
		switch op {
		case "==":
			return fmt.Sprintf("%v", raw) == right
		case "!=":
			return fmt.Sprintf("%v", raw) != right
		case ">", "<", ">=", "<=":
			l, lErr := toFloat(raw)
			r, rErr := strconv.ParseFloat(right, 64)
			if lErr != nil || rErr != nil {
				return false
			}
			switch op {
			case ">":
				return l > r
			case "<":
				return l < r
			case ">=":
				return l >= r
			case "<=":
				return l <= r
			}
		}
	}
	return false
}

func toFloat(v interface{}) (float64, error) {
	switch t := v.(type) {
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case float32:
		return float64(t), nil
	case float64:
		return t, nil
	case string:
		return strconv.ParseFloat(t, 64)
	default:
		return 0, fmt.Errorf("unsupported type")
	}
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
