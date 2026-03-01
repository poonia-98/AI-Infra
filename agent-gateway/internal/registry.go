package internal

import (
	"strings"
	"sync"
	"time"
)

type AgentRecord struct {
	AgentID   string            `json:"agent_id"`
	Status    string            `json:"status"`
	Groups    []string          `json:"groups,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	LastSeen  time.Time         `json:"last_seen"`
	Reachable bool              `json:"reachable"`
}

type Registry struct {
	mu       sync.RWMutex
	agents   map[string]AgentRecord
	byGroup  map[string]map[string]struct{}
}

func NewRegistry() *Registry {
	return &Registry{
		agents:  map[string]AgentRecord{},
		byGroup: map[string]map[string]struct{}{},
	}
}

func (r *Registry) Register(record AgentRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()

	record.AgentID = strings.TrimSpace(record.AgentID)
	if record.AgentID == "" {
		return
	}
	record.LastSeen = time.Now().UTC()
	if record.Status == "" {
		record.Status = "running"
	}
	record.Reachable = true

	if current, exists := r.agents[record.AgentID]; exists {
		r.removeGroupsLocked(current.AgentID, current.Groups)
	}
	r.agents[record.AgentID] = record
	r.addGroupsLocked(record.AgentID, record.Groups)
}

func (r *Registry) Heartbeat(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.agents[agentID]
	if !ok {
		return
	}
	record.LastSeen = time.Now().UTC()
	record.Reachable = true
	r.agents[agentID] = record
}

func (r *Registry) Unregister(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.agents[agentID]
	if !ok {
		return
	}
	r.removeGroupsLocked(agentID, record.Groups)
	delete(r.agents, agentID)
}

func (r *Registry) Get(agentID string) (AgentRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.agents[agentID]
	return record, ok
}

func (r *Registry) List() []AgentRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentRecord, 0, len(r.agents))
	for _, record := range r.agents {
		out = append(out, record)
	}
	return out
}

func (r *Registry) DiscoverByGroup(group string) []AgentRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := r.byGroup[group]
	if len(ids) == 0 {
		return []AgentRecord{}
	}
	out := make([]AgentRecord, 0, len(ids))
	for id := range ids {
		record, ok := r.agents[id]
		if !ok {
			continue
		}
		if record.Status == "running" && record.Reachable {
			out = append(out, record)
		}
	}
	return out
}

func (r *Registry) PruneStale(staleAfter time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	var removed []string
	for id, record := range r.agents {
		if staleAfter > 0 && now.Sub(record.LastSeen) <= staleAfter {
			continue
		}
		r.removeGroupsLocked(id, record.Groups)
		delete(r.agents, id)
		removed = append(removed, id)
	}
	return removed
}

func (r *Registry) addGroupsLocked(agentID string, groups []string) {
	for _, group := range groups {
		if group == "" {
			continue
		}
		if _, ok := r.byGroup[group]; !ok {
			r.byGroup[group] = map[string]struct{}{}
		}
		r.byGroup[group][agentID] = struct{}{}
	}
}

func (r *Registry) removeGroupsLocked(agentID string, groups []string) {
	for _, group := range groups {
		members, ok := r.byGroup[group]
		if !ok {
			continue
		}
		delete(members, agentID)
		if len(members) == 0 {
			delete(r.byGroup, group)
		}
	}
}
