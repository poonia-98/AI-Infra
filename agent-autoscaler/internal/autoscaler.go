package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

type Autoscaler struct {
	db       *sql.DB
	provider MetricsProvider
	logger   *slog.Logger
	nc       *natsgo.Conn
}

func NewAutoscaler(db *sql.DB, provider MetricsProvider, logger *slog.Logger, nc *natsgo.Conn) *Autoscaler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Autoscaler{
		db:       db,
		provider: provider,
		logger:   logger,
		nc:       nc,
	}
}

func (a *Autoscaler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.EvaluateOnce(ctx)
		}
	}
}

func (a *Autoscaler) EvaluateOnce(ctx context.Context) {
	policies, err := a.loadPolicies(ctx)
	if err != nil {
		a.logger.Error("failed to load autoscaling policies", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		snapshot, err := a.provider.GetSnapshot(ctx, policy.AgentID)
		if err != nil {
			a.logger.Warn("failed to fetch metrics", "agent_id", policy.AgentID, "error", err)
			continue
		}
		decision := EvaluatePolicy(policy, snapshot, now)
		if !decision.Scale {
			continue
		}
		if err := a.applyScaleDecision(ctx, policy, decision); err != nil {
			a.logger.Error("failed to apply scaling decision", "policy_id", policy.ID, "error", err)
		}
	}
}

func (a *Autoscaler) applyScaleDecision(ctx context.Context, policy ScalePolicy, decision ScaleDecision) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	_, err = tx.ExecContext(ctx, `
		UPDATE autoscale_policies
		SET current_replicas=$1, last_scale_at=NOW(), updated_at=NOW()
		WHERE id=$2::uuid`, decision.Desired, policy.ID)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO autoscale_events
			(policy_id, agent_id, direction, from_replicas, to_replicas, trigger_metric, trigger_value)
		VALUES
			($1::uuid, $2::uuid, $3, $4, $5, $6, $7)`,
		policy.ID, policy.AgentID, decision.Direction, decision.From, decision.Desired, decision.Trigger, decision.Metric)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	a.publishScaleEvent(policy.AgentID, decision)
	return nil
}

func (a *Autoscaler) loadPolicies(ctx context.Context) ([]ScalePolicy, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id::text, agent_id::text, enabled, min_replicas, max_replicas,
		       scale_up_threshold, scale_down_threshold, scale_up_cooldown_s,
		       scale_down_cooldown_s, scale_increment, current_replicas, last_scale_at
		FROM autoscale_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScalePolicy
	for rows.Next() {
		var item ScalePolicy
		if err := rows.Scan(
			&item.ID,
			&item.AgentID,
			&item.Enabled,
			&item.MinReplicas,
			&item.MaxReplicas,
			&item.ScaleUpThreshold,
			&item.ScaleDownThreshold,
			&item.ScaleUpCooldownSec,
			&item.ScaleDownCooldownSec,
			&item.ScaleIncrement,
			&item.CurrentReplicas,
			&item.LastScaleAt,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *Autoscaler) publishScaleEvent(agentID string, decision ScaleDecision) {
	if a.nc == nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"agent_id":        agentID,
		"direction":       decision.Direction,
		"from_replicas":   decision.From,
		"desired_replicas": decision.Desired,
		"trigger_metric":  decision.Trigger,
		"trigger_value":   decision.Metric,
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	_ = a.nc.Publish("autoscaler.events", payload)
	_ = a.nc.Publish("agent.scale."+decision.Direction, payload)
}

func (a *Autoscaler) ValidateDependencies() error {
	if a.db == nil {
		return errors.New("db is required")
	}
	if a.provider == nil {
		return errors.New("metrics provider is required")
	}
	return nil
}
