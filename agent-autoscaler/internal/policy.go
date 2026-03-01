package internal

import "time"

type ScalePolicy struct {
	ID                   string     `json:"id"`
	AgentID              string     `json:"agent_id"`
	Enabled              bool       `json:"enabled"`
	MinReplicas          int        `json:"min_replicas"`
	MaxReplicas          int        `json:"max_replicas"`
	ScaleUpThreshold     float64    `json:"scale_up_threshold"`
	ScaleDownThreshold   float64    `json:"scale_down_threshold"`
	ScaleUpCooldownSec   int        `json:"scale_up_cooldown_s"`
	ScaleDownCooldownSec int        `json:"scale_down_cooldown_s"`
	ScaleIncrement       int        `json:"scale_increment"`
	CurrentReplicas      int        `json:"current_replicas"`
	LastScaleAt          *time.Time `json:"last_scale_at,omitempty"`
}

type ScaleDecision struct {
	Scale      bool    `json:"scale"`
	Direction  string  `json:"direction,omitempty"`
	Desired    int     `json:"desired"`
	Reason     string  `json:"reason,omitempty"`
	Trigger    string  `json:"trigger"`
	Metric     float64 `json:"metric"`
	From       int     `json:"from"`
	CooldownOK bool    `json:"cooldown_ok"`
}

func EvaluatePolicy(policy ScalePolicy, metrics MetricSnapshot, now time.Time) ScaleDecision {
	queueDecision := evaluateMetric(policy, "queue_depth", metrics.QueueDepth, now)
	if queueDecision.Scale {
		return queueDecision
	}
	cpuDecision := evaluateMetric(policy, "cpu_percent", metrics.CPUUsage, now)
	if cpuDecision.Scale {
		return cpuDecision
	}
	memDecision := evaluateMetric(policy, "memory_percent", metrics.MemoryUsage, now)
	if memDecision.Scale {
		return memDecision
	}
	latDecision := evaluateMetric(policy, "latency_ms", metrics.LatencyMS, now)
	if latDecision.Scale {
		return latDecision
	}
	return ScaleDecision{
		Scale:      false,
		Desired:    policy.CurrentReplicas,
		Trigger:    "none",
		Metric:     0,
		From:       policy.CurrentReplicas,
		CooldownOK: true,
	}
}

func evaluateMetric(policy ScalePolicy, trigger string, value float64, now time.Time) ScaleDecision {
	increment := policy.ScaleIncrement
	if increment <= 0 {
		increment = 1
	}

	upAllowed := isCooldownElapsed(policy.LastScaleAt, now, policy.ScaleUpCooldownSec)
	downAllowed := isCooldownElapsed(policy.LastScaleAt, now, policy.ScaleDownCooldownSec)

	if value >= policy.ScaleUpThreshold && policy.CurrentReplicas < policy.MaxReplicas {
		desired := policy.CurrentReplicas + increment
		if desired > policy.MaxReplicas {
			desired = policy.MaxReplicas
		}
		return ScaleDecision{
			Scale:      upAllowed,
			Direction:  "up",
			Desired:    desired,
			Reason:     "threshold_exceeded",
			Trigger:    trigger,
			Metric:     value,
			From:       policy.CurrentReplicas,
			CooldownOK: upAllowed,
		}
	}

	if value <= policy.ScaleDownThreshold && policy.CurrentReplicas > policy.MinReplicas {
		desired := policy.CurrentReplicas - increment
		if desired < policy.MinReplicas {
			desired = policy.MinReplicas
		}
		return ScaleDecision{
			Scale:      downAllowed,
			Direction:  "down",
			Desired:    desired,
			Reason:     "threshold_below",
			Trigger:    trigger,
			Metric:     value,
			From:       policy.CurrentReplicas,
			CooldownOK: downAllowed,
		}
	}

	return ScaleDecision{
		Scale:      false,
		Desired:    policy.CurrentReplicas,
		Trigger:    trigger,
		Metric:     value,
		From:       policy.CurrentReplicas,
		CooldownOK: true,
	}
}

func isCooldownElapsed(last *time.Time, now time.Time, cooldownSeconds int) bool {
	if cooldownSeconds <= 0 || last == nil {
		return true
	}
	return now.Sub(*last) >= time.Duration(cooldownSeconds)*time.Second
}
