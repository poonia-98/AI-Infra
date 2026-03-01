package internal

import (
	"context"
	"errors"
	"strings"
	"time"
)

type RouteRequest struct {
	Message Message `json:"message"`
}

type RouteResult struct {
	RoutedTo int    `json:"routed_to"`
	Status   string `json:"status"`
}

type Router struct {
	registry   *Registry
	dispatcher *Dispatcher
}

func NewRouter(registry *Registry, dispatcher *Dispatcher) *Router {
	return &Router{
		registry:   registry,
		dispatcher: dispatcher,
	}
}

func (r *Router) RegisterAgent(agentID string, groups []string, metadata map[string]string) {
	r.registry.Register(AgentRecord{
		AgentID:  agentID,
		Status:   "running",
		Groups:   groups,
		Metadata: metadata,
	})
}

func (r *Router) UnregisterAgent(agentID string) {
	r.registry.Unregister(agentID)
}

func (r *Router) Route(ctx context.Context, req RouteRequest) (RouteResult, error) {
	msg := req.Message
	if strings.TrimSpace(msg.FromAgentID) == "" {
		return RouteResult{}, errors.New("from_agent_id is required")
	}
	if msg.ExpiresAt != nil && msg.ExpiresAt.Before(time.Now().UTC()) {
		return RouteResult{Status: "expired", RoutedTo: 0}, nil
	}
	count, err := r.dispatcher.Dispatch(ctx, msg)
	if err != nil {
		return RouteResult{}, err
	}
	status := "routed"
	if count == 0 {
		status = "no_recipients"
	}
	return RouteResult{RoutedTo: count, Status: status}, nil
}

func (r *Router) SpawnAgent(ctx context.Context, req SpawnRequest) error {
	return r.dispatcher.Spawn(ctx, req)
}

func (r *Router) Discover(agentID, group string) []AgentRecord {
	if strings.TrimSpace(agentID) != "" {
		if record, ok := r.registry.Get(agentID); ok {
			return []AgentRecord{record}
		}
		return []AgentRecord{}
	}
	if strings.TrimSpace(group) != "" {
		return r.registry.DiscoverByGroup(group)
	}
	return r.registry.List()
}
