package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type MetricSnapshot struct {
	QueueDepth  float64 `json:"queue_depth"`
	CPUUsage    float64 `json:"cpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`
	LatencyMS   float64 `json:"latency_ms"`
}

type MetricsProvider interface {
	GetSnapshot(ctx context.Context, agentID string) (MetricSnapshot, error)
}

type SQLMetricsProvider struct {
	db         *sql.DB
	httpClient *http.Client
	metricsURL string
}

func NewSQLMetricsProvider(db *sql.DB, metricsURL string, timeout time.Duration) *SQLMetricsProvider {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &SQLMetricsProvider{
		db:         db,
		httpClient: &http.Client{Timeout: timeout},
		metricsURL: metricsURL,
	}
}

func (m *SQLMetricsProvider) GetSnapshot(ctx context.Context, agentID string) (MetricSnapshot, error) {
	queueDepth, err := m.queueDepth(ctx, agentID)
	if err != nil {
		return MetricSnapshot{}, err
	}
	cpu, _ := m.latestMetric(ctx, agentID, "cpu_percent")
	memory, _ := m.latestMetric(ctx, agentID, "memory_percent")
	latency, _ := m.latestMetric(ctx, agentID, "latency_ms")
	return MetricSnapshot{
		QueueDepth:  queueDepth,
		CPUUsage:    cpu,
		MemoryUsage: memory,
		LatencyMS:   latency,
	}, nil
}

func (m *SQLMetricsProvider) queueDepth(ctx context.Context, agentID string) (float64, error) {
	var value float64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*)::float8
		FROM execution_queues
		WHERE agent_id=$1::uuid AND status='queued'`, agentID).Scan(&value)
	return value, err
}

func (m *SQLMetricsProvider) latestMetric(ctx context.Context, agentID, metric string) (float64, error) {
	if m.metricsURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/metrics/agent/%s/%s/latest", m.metricsURL, agentID, metric), nil)
		if err == nil {
			resp, err := m.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				var payload struct {
					Value float64 `json:"value"`
				}
				if json.NewDecoder(resp.Body).Decode(&payload) == nil {
					return payload.Value, nil
				}
			}
		}
	}
	var value float64
	err := m.db.QueryRowContext(ctx, `
		SELECT metric_value
		FROM metrics
		WHERE agent_id=$1::uuid AND metric_name=$2
		ORDER BY timestamp DESC
		LIMIT 1`, agentID, metric).Scan(&value)
	return value, err
}
