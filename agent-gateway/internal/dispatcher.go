package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

type Message struct {
	ID            string                 `json:"id,omitempty"`
	FromAgentID   string                 `json:"from_agent_id"`
	ToAgentID     string                 `json:"to_agent_id,omitempty"`
	ToGroup       string                 `json:"to_group,omitempty"`
	MessageType   string                 `json:"message_type"`
	Subject       string                 `json:"subject,omitempty"`
	Payload       map[string]interface{} `json:"payload"`
	CorrelationID string                 `json:"correlation_id,omitempty"`
	ReplyTo       string                 `json:"reply_to,omitempty"`
	Priority      int                    `json:"priority"`
	ExpiresAt     *time.Time             `json:"expires_at,omitempty"`
}

type SpawnRequest struct {
	ParentAgentID  string                 `json:"parent_agent_id"`
	Name           string                 `json:"name"`
	Image          string                 `json:"image"`
	Config         map[string]interface{} `json:"config"`
	EnvVars        map[string]string      `json:"env_vars"`
	SpawnReason    string                 `json:"spawn_reason"`
	OrganisationID string                 `json:"organisation_id,omitempty"`
}

type Dispatcher struct {
	nc       *natsgo.Conn
	registry *Registry
}

func NewDispatcher(nc *natsgo.Conn, registry *Registry) *Dispatcher {
	return &Dispatcher{
		nc:       nc,
		registry: registry,
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, msg Message) (int, error) {
	if d.nc == nil {
		return 0, errors.New("nats connection not available")
	}
	if msg.FromAgentID == "" {
		return 0, errors.New("from_agent_id is required")
	}
	if msg.MessageType == "" {
		msg.MessageType = "task"
	}
	if msg.Priority == 0 {
		msg.Priority = 5
	}
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), msg.FromAgentID)
	}
	if msg.Payload == nil {
		msg.Payload = map[string]interface{}{}
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}

	if msg.ToAgentID != "" {
		subject := "agent.inbox." + msg.ToAgentID
		if err := d.publishWithContext(ctx, subject, data); err != nil {
			return 0, err
		}
		return 1, nil
	}

	if msg.ToGroup != "" {
		recipients := d.registry.DiscoverByGroup(msg.ToGroup)
		if len(recipients) == 0 {
			return 0, nil
		}
		count := 0
		for _, recipient := range recipients {
			if recipient.AgentID == msg.FromAgentID {
				continue
			}
			subject := "agent.inbox." + recipient.AgentID
			if err := d.publishWithContext(ctx, subject, data); err == nil {
				count++
			}
		}
		return count, nil
	}

	if err := d.publishWithContext(ctx, "agent.broadcast", data); err != nil {
		return 0, err
	}
	return len(d.registry.List()), nil
}

func (d *Dispatcher) Spawn(ctx context.Context, request SpawnRequest) error {
	if d.nc == nil {
		return errors.New("nats connection not available")
	}
	if request.ParentAgentID == "" || request.Name == "" || request.Image == "" {
		return errors.New("parent_agent_id, name, and image are required")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return d.publishWithContext(ctx, "agent.spawn", payload)
}

func (d *Dispatcher) publishWithContext(ctx context.Context, subject string, payload []byte) error {
	done := make(chan error, 1)
	go func() {
		done <- d.nc.Publish(subject, payload)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}
