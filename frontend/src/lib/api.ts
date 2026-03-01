import { getAuthHeaders, silentRefresh } from '@/lib/auth';

const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8000';

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...getAuthHeaders(),
    ...(options.headers as Record<string, string> || {}),
  };

  const res = await fetch(`${API_URL}${path}`, {
    ...options,
    credentials: 'include',
    headers,
  });

  // Auto-refresh on 401 and retry once
  if (res.status === 401) {
    const refreshed = await silentRefresh();
    if (refreshed) {
      const retry = await fetch(`${API_URL}${path}`, {
        ...options,
        credentials: 'include',
        headers: { 'Content-Type': 'application/json', ...getAuthHeaders() },
      });
      if (retry.ok) return retry.json();
    }
    // Redirect to login
    if (typeof window !== 'undefined') window.location.href = '/login';
    throw new Error('Session expired');
  }

  if (!res.ok) {
    const err = await res.json().catch(() => ({ detail: res.statusText }));
    throw new Error(err.detail || `HTTP ${res.status}`);
  }
  return res.json();
}

// ── Types ─────────────────────────────────────────────────────────────────────

export interface Agent {
  id: string; name: string; description: string | null;
  agent_type: string; config: Record<string, unknown>; image: string;
  status: string; container_id: string | null; container_name: string | null;
  cpu_limit: number; memory_limit: string; env_vars: Record<string, string>;
  labels: Record<string, string>; auto_restart: boolean; restart_count: number;
  last_error: string | null; created_at: string; updated_at: string;
}
export interface Execution {
  id: string; agent_id: string; status: string;
  input: Record<string, unknown>; output: Record<string, unknown>;
  error: string | null; started_at: string | null; completed_at: string | null;
  duration_ms: number | null; exit_code: number | null; created_at: string;
}
export interface Log {
  id: string; agent_id: string; execution_id: string | null;
  level: string; message: string; source: string | null;
  metadata: Record<string, unknown>; timestamp: string;
}
export interface Event {
  id: string; agent_id: string; event_type: string;
  payload: Record<string, unknown>; source: string | null;
  level: string; created_at: string;
}
export interface Metric {
  id: string; agent_id: string; metric_name: string;
  metric_value: number; labels: Record<string, unknown>; timestamp: string;
}
export interface Alert {
  id: string; rule_id: string | null; agent_id: string | null;
  metric_name: string; metric_value: number; threshold: number;
  severity: string; message: string; resolved: boolean;
  resolved_at: string | null; created_at: string;
}
export interface AlertCounts {
  total: number;
  active: number;
  resolved: number;
  critical: number;
  warning: number;
  info: number;
}
export interface SystemHealth {
  status: string; total_agents: number; running_agents: number;
  failed_agents: number; total_executions: number; active_executions: number;
  active_alerts: number; uptime_seconds: number; version: string;
  features: Record<string, boolean>;
  services: { database: string; redis: string; nats: string };
}

// Cloud platform types
export interface BillingPlan {
  id: string; name: string; display_name: string; price_monthly_usd: number;
  price_annual_usd: number; max_agents: number; max_executions_day: number;
  max_api_rpm: number; features: Record<string, unknown>;
}
export interface Subscription {
  id: string; org_id: string; plan_name: string; billing_cycle: string;
  status: string; current_period_start: string; current_period_end: string;
  trial_ends_at: string | null; cancelled_at: string | null;
}
export interface UsageRecord {
  id: string; org_id: string; resource_type: string;
  quantity: number; unit: string; period_start: string; period_end: string;
}
export interface Region {
  id: string; name: string; display_name: string; provider: string;
  status: string; is_primary: boolean; endpoint_url: string;
  agent_count: number; cluster_count: number;
}
export interface SandboxPolicy {
  id: string; org_id: string; runtime: string; policy_name: string;
  max_cpu_cores: number; max_memory_mb: number; network_enabled: boolean;
  allowed_syscalls: string[]; enabled: boolean;
}
export interface SimulationEnvironment {
  id: string; org_id: string; name: string; description: string;
  scenario_type: string; config: Record<string, unknown>; status: string;
}
export interface KnowledgeBase {
  id: string; org_id: string; name: string; description: string;
  chunk_count: number; embedding_model: string; created_at: string;
}
export interface FederationPeer {
  id: string; org_id: string; peer_name: string; endpoint_url: string;
  status: string; trust_level: string; last_seen: string | null;
}
export interface EvolutionExperiment {
  id: string; org_id: string; name: string; status: string;
  generation: number; population_size: number; best_fitness: number | null;
  created_at: string;
}

// ── API client ────────────────────────────────────────────────────────────────

export const api = {
  system: {
    health: (): Promise<SystemHealth> => request('/api/v1/system/health'),
  },

  agents: {
    list: (skip = 0, limit = 100): Promise<Agent[]> =>
      request(`/api/v1/agents?skip=${skip}&limit=${limit}`),
    get: (id: string): Promise<Agent> => request(`/api/v1/agents/${id}`),
    create: (data: Partial<Agent>): Promise<Agent> =>
      request('/api/v1/agents', { method: 'POST', body: JSON.stringify(data) }),
    update: (id: string, data: Partial<Agent>): Promise<Agent> =>
      request(`/api/v1/agents/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id: string): Promise<void> =>
      request(`/api/v1/agents/${id}`, { method: 'DELETE' }),
    start: (id: string) => request<{ success: boolean }>(`/api/v1/agents/${id}/start`, { method: 'POST' }),
    stop: (id: string) => request<{ success: boolean }>(`/api/v1/agents/${id}/stop`, { method: 'POST' }),
    restart: (id: string) => request<{ success: boolean }>(`/api/v1/agents/${id}/restart`, { method: 'POST' }),
    logs: (id: string, limit = 200, level?: string): Promise<Log[]> => {
      const p = new URLSearchParams({ limit: String(limit) });
      if (level) p.set('level', level);
      return request(`/api/v1/agents/${id}/logs?${p}`);
    },
    executions: (id: string, limit = 50): Promise<Execution[]> =>
      request(`/api/v1/agents/${id}/executions?limit=${limit}`),
    events: (id: string, limit = 100): Promise<Event[]> =>
      request(`/api/v1/agents/${id}/events?limit=${limit}`),
    metrics: (id: string, limit = 200): Promise<Metric[]> =>
      request(`/api/v1/agents/${id}/metrics?limit=${limit}`),
  },

  alerts: {
    list: (resolved?: boolean, limit = 100): Promise<Alert[]> => {
      const p = new URLSearchParams({ limit: String(limit) });
      if (resolved !== undefined) p.set('resolved', String(resolved));
      return request(`/api/v1/alerts?${p}`);
    },
    resolve: (id: string): Promise<Alert> =>
      request(`/api/v1/alerts/${id}/resolve`, { method: 'POST' }),
    counts: (): Promise<AlertCounts> =>
      request('/api/v1/alerts/counts'),
    rules: (limit = 50) => request<any[]>(`/api/v1/alert-rules?limit=${limit}`),
    createRule: (data: Record<string, unknown>) =>
      request<any>('/api/v1/alert-rules', { method: 'POST', body: JSON.stringify(data) }),
  },

  // ── Cloud Platform Extension APIs ─────────────────────────────────────────

  billing: {
    plans: (): Promise<BillingPlan[]> => request('/api/v1/cloud/billing/plans'),
    subscription: (orgId: string): Promise<Subscription> =>
      request(`/api/v1/cloud/subscriptions/${orgId}`),
    usage: (orgId: string, period?: string): Promise<UsageRecord[]> => {
      const p = new URLSearchParams();
      if (period) p.set('period', period);
      return request(`/api/v1/cloud/usage/${orgId}?${p}`);
    },
    invoices: (orgId: string, limit = 20) =>
      request<any[]>(`/api/v1/cloud/invoices?org_id=${orgId}&limit=${limit}`),
    upgrade: (orgId: string, planName: string, billingCycle: string) =>
      request(`/api/v1/cloud/subscriptions/${orgId}/upgrade`, {
        method: 'PATCH',
        body: JSON.stringify({ plan_name: planName, billing_cycle: billingCycle }),
      }),
  },

  observability: {
    traces: (agentId?: string, limit = 50) => {
      const p = new URLSearchParams({ limit: String(limit) });
      if (agentId) p.set('agent_id', agentId);
      return request<any[]>(`/api/v1/observability/traces?${p}`);
    },
    performance: (agentId?: string, timeRange = '1h') => {
      const p = new URLSearchParams({ time_range: timeRange });
      if (agentId) p.set('agent_id', agentId);
      return request<any[]>(`/api/v1/observability/performance?${p}`);
    },
    diagnostics: (agentId: string) =>
      request<any>(`/api/v1/observability/diagnostics/${agentId}`),
    lineage: (agentId: string) =>
      request<any>(`/api/v1/observability/lineage/${agentId}`),
  },

  sandbox: {
    policies: (): Promise<SandboxPolicy[]> => request('/api/v1/sandbox/policies'),
    createPolicy: (data: Partial<SandboxPolicy>): Promise<SandboxPolicy> =>
      request('/api/v1/sandbox/policies', { method: 'POST', body: JSON.stringify(data) }),
    violations: (limit = 50) => request<any[]>(`/api/v1/sandbox/violations?limit=${limit}`),
    runtimes: () => request<{ runtimes: string[] }>('/api/v1/sandbox/runtimes'),
  },

  simulation: {
    environments: (): Promise<SimulationEnvironment[]> =>
      request('/api/v1/simulation/environments'),
    createEnvironment: (data: Partial<SimulationEnvironment>) =>
      request<SimulationEnvironment>('/api/v1/simulation/environments', {
        method: 'POST', body: JSON.stringify(data),
      }),
    runs: (envId: string, limit = 20) =>
      request<any[]>(`/api/v1/simulation/environments/${envId}/runs?limit=${limit}`),
    startRun: (envId: string, config?: Record<string, unknown>) =>
      request<any>(`/api/v1/simulation/environments/${envId}/run`, {
        method: 'POST', body: JSON.stringify(config || {}),
      }),
  },

  regions: {
    list: (): Promise<Region[]> => request('/api/v1/regions'),
    get: (id: string): Promise<Region> => request(`/api/v1/regions/${id}`),
    placements: (limit = 50) => request<any[]>(`/api/v1/regions/placements?limit=${limit}`),
    failovers: (limit = 20) => request<any[]>(`/api/v1/regions/failovers?limit=${limit}`),
    clusters: (regionId: string) => request<any[]>(`/api/v1/regions/${regionId}/clusters`),
  },

  federation: {
    peers: (): Promise<FederationPeer[]> => request('/api/v1/federation/peers'),
    addPeer: (data: Partial<FederationPeer>): Promise<FederationPeer> =>
      request('/api/v1/federation/peers', { method: 'POST', body: JSON.stringify(data) }),
    tasks: (limit = 50) => request<any[]>(`/api/v1/federation/tasks?limit=${limit}`),
    dispatch: (peerId: string, task: Record<string, unknown>) =>
      request<any>(`/api/v1/federation/peers/${peerId}/dispatch`, {
        method: 'POST', body: JSON.stringify(task),
      }),
  },

  evolution: {
    experiments: (): Promise<EvolutionExperiment[]> => request('/api/v1/evolution/experiments'),
    createExperiment: (data: Partial<EvolutionExperiment>): Promise<EvolutionExperiment> =>
      request('/api/v1/evolution/experiments', { method: 'POST', body: JSON.stringify(data) }),
    genomes: (experimentId: string, limit = 20) =>
      request<any[]>(`/api/v1/evolution/experiments/${experimentId}/genomes?limit=${limit}`),
    clones: (limit = 20) => request<any[]>(`/api/v1/evolution/clones?limit=${limit}`),
  },

  knowledge: {
    bases: (): Promise<KnowledgeBase[]> => request('/api/v1/knowledge/bases'),
    createBase: (data: Partial<KnowledgeBase>): Promise<KnowledgeBase> =>
      request('/api/v1/knowledge/bases', { method: 'POST', body: JSON.stringify(data) }),
    search: (baseId: string, query: string, topK = 10) =>
      request<any[]>(`/api/v1/knowledge/bases/${baseId}/search`, {
        method: 'POST', body: JSON.stringify({ query, top_k: topK }),
      }),
    chunks: (baseId: string, limit = 50) =>
      request<any[]>(`/api/v1/knowledge/bases/${baseId}/chunks?limit=${limit}`),
  },

  vault: {
    secrets: () => request<any[]>('/api/v1/vault/secrets'),
    createSecret: (data: { name: string; value: string; description?: string; tags?: string[] }) =>
      request<any>('/api/v1/vault/secrets', { method: 'POST', body: JSON.stringify(data) }),
    rotateKey: (id: string) =>
      request<any>(`/api/v1/vault/secrets/${id}/rotate`, { method: 'POST' }),
    accessLog: (id: string, limit = 50) =>
      request<any[]>(`/api/v1/vault/secrets/${id}/access-log?limit=${limit}`),
    keys: () => request<any[]>('/api/v1/vault/keys'),
  },
  brain: {
    summary: () => request<any>('/api/v1/brain/state/summary'),
    state: () => request<any>('/api/v1/brain/state'),
    agents: () => request<any[]>('/api/v1/brain/state/agents').then((r: any) => r?.agents || r || []),
    services: () => request<any[]>('/api/v1/brain/services'),
    nodes: () => request<any[]>('/api/v1/brain/nodes'),
    decisions: () => request<any[]>('/api/v1/brain/decisions').then((r: any) => r?.decisions || r || []),
    scale: (agentId: string, direction: string, replicas = 1) =>
      request<any>('/api/v1/brain/scale', { method: 'POST', body: JSON.stringify({ agent_id: agentId, direction, replicas }) }),
    reconcile: () => request<any>('/api/v1/brain/reconcile', { method: 'POST' }),
    placement: (cpu = 0.5, memoryMb = 256, region = '') =>
      request<any>('/api/v1/brain/placement', { method: 'POST', body: JSON.stringify({ cpu_request: cpu, memory_request_mb: memoryMb, region }) }),
    health: () => request<any>('/api/v1/brain/health'),
  },

  memory: {
    list: (agentId: string, memoryType?: string, limit = 100) => {
      const params = new URLSearchParams({ agent_id: agentId, limit: String(limit) });
      if (memoryType) params.set('memory_type', memoryType);
      return request<any[]>(`/api/v1/memory?${params}`);
    },
    store: (agentId: string, content: string, memoryType = 'short_term', key?: string, importance = 0.5) =>
      request<any>('/api/v1/memory', { method: 'POST', body: JSON.stringify({ agent_id: agentId, content, memory_type: memoryType, key, importance }) }),
    search: (agentId: string, query: string, limit = 10) =>
      request<any[]>(`/api/v1/memory/search`, { method: 'POST', body: JSON.stringify({ agent_id: agentId, query, limit }) }),
    context: (agentId: string, query: string) =>
      request<any>(`/api/v1/memory/context`, { method: 'POST', body: JSON.stringify({ agent_id: agentId, query }) }),
    delete: (agentId: string, memoryId: string) =>
      request<any>(`/api/v1/memory/${memoryId}?agent_id=${agentId}`, { method: 'DELETE' }),
    stats: (agentId: string) =>
      request<any>(`/api/v1/memory/stats?agent_id=${agentId}`),
    prune: (agentId: string) =>
      request<any>('/api/v1/memory/prune', { method: 'POST', body: JSON.stringify({ agent_id: agentId }) }),
  },

  executions: {
    list: (limit = 50) => request<any[]>(`/api/v1/executions?limit=${limit}`),
    get: (id: string) => request<any>(`/api/v1/executions/${id}`),
  },

};