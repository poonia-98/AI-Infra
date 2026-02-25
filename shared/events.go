// Package events defines the shared event schema used across all Go services.
// Schema version: 1.0.0
package events

import "time"

// EventType constants — must match shared/events.json
const (
	EventAgentCreated   = "agent.created"
	EventAgentStarted   = "agent.started"
	EventAgentStopped   = "agent.stopped"
	EventAgentFailed    = "agent.failed"
	EventAgentCompleted = "agent.completed"
	EventAgentDeleted   = "agent.deleted"
	EventAgentHeartbeat = "agent.heartbeat"
)

// NATS subject constants
const (
	SubjectAgentsEvents  = "agents.events"
	SubjectLogsStream    = "logs.stream"
	SubjectEventsStream  = "events.stream"
	SubjectMetricsStream = "metrics.stream"
)

// BaseEvent is the canonical agent lifecycle event envelope.
type BaseEvent struct {
	EventType   string                 `json:"event_type"`
	AgentID     string                 `json:"agent_id"`
	ExecutionID string                 `json:"execution_id,omitempty"`
	Timestamp   string                 `json:"timestamp"`
	Source      string                 `json:"source"`
	Payload     map[string]interface{} `json:"payload,omitempty"`
}

// NewBaseEvent constructs a BaseEvent with current UTC timestamp.
func NewBaseEvent(eventType, agentID, source string) BaseEvent {
	return BaseEvent{
		EventType: eventType,
		AgentID:   agentID,
		Source:    source,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Payload:   make(map[string]interface{}),
	}
}

// LogEvent is the canonical log entry schema.
type LogEvent struct {
	AgentID     string                 `json:"agent_id"`
	ExecutionID string                 `json:"execution_id,omitempty"`
	Message     string                 `json:"message"`
	Level       string                 `json:"level"`
	Source      string                 `json:"source,omitempty"`
	Timestamp   string                 `json:"timestamp"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

// NewLogEvent creates a LogEvent at the current UTC time.
func NewLogEvent(agentID, message, level, source string) LogEvent {
	return LogEvent{
		AgentID:   agentID,
		Message:   message,
		Level:     level,
		Source:    source,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// MetricEvent is the canonical metrics schema.
type MetricEvent struct {
	AgentID     string                 `json:"agent_id"`
	MetricName  string                 `json:"metric_name"`
	MetricValue float64                `json:"metric_value"`
	Labels      map[string]interface{} `json:"labels,omitempty"`
	Timestamp   string                 `json:"timestamp"`
}

// NewMetricEvent creates a MetricEvent at the current UTC time.
func NewMetricEvent(agentID, name string, value float64) MetricEvent {
	return MetricEvent{
		AgentID:     agentID,
		MetricName:  name,
		MetricValue: value,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
}

// AgentStartedPayload holds container info for agent.started events.
type AgentStartedPayload struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
}

// AgentStoppedPayload holds exit info for agent.stopped events.
type AgentStoppedPayload struct {
	ContainerID string `json:"container_id"`
	ExitCode    *int   `json:"exit_code,omitempty"`
}

// AgentFailedPayload holds error info for agent.failed events.
type AgentFailedPayload struct {
	Error       string `json:"error"`
	ContainerID string `json:"container_id,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
}

// HeartbeatPayload holds runtime stats for agent.heartbeat events.
type HeartbeatPayload struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryMB      float64 `json:"memory_mb"`
	MemoryPercent float64 `json:"memory_percent"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}