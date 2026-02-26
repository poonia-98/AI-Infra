const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8000';

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const res = await fetch(`${API_URL}${path}`, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...options.headers },
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ detail: res.statusText }));
    throw new Error(err.detail || `HTTP ${res.status}`);
  }
  return res.json();
}

export interface Agent {
  id: string; name: string; description: string | null; agent_type: string;
  config: Record<string, unknown>; image: string; status: string;
  container_id: string | null; container_name: string | null;
  cpu_limit: number; memory_limit: string;
  env_vars: Record<string, string>; labels: Record<string, string>;
  created_at: string; updated_at: string;
}
export interface Execution {
  id: string; agent_id: string; status: string;
  input: Record<string, unknown>; output: Record<string, unknown>;
  error: string | null; started_at: string | null;
  completed_at: string | null; duration_ms: number | null;
  exit_code: number | null; created_at: string;
}
export interface Log {
  id: string; agent_id: string; execution_id: string | null;
  level: string; message: string; source: string | null;
  metadata: Record<string, unknown>; timestamp: string;
}
export interface Event {
  id: string; agent_id: string; event_type: string;
  payload: Record<string, unknown>; source: string | null; created_at: string;
}
export interface Metric {
  id: string; agent_id: string; metric_name: string;
  metric_value: number; labels: Record<string, unknown>; timestamp: string;
}
export interface Alert {
  id: string; agent_id: string | null; alert_type: string;
  severity: string; title: string; message: string; status: string;
  value: number | null; threshold: number | null;
  resolved_at: string | null; created_at: string; updated_at: string;
}
export interface AlertCounts { active: number; critical: number; warnings: number; resolved: number; }
export interface ServiceStatuses { database?: string; redis?: string; nats?: string; }
export interface SystemHealth {
  status: string; total_agents: number; running_agents: number;
  failed_agents: number; total_executions: number; active_executions: number;
  uptime_seconds: number; services?: ServiceStatuses; error?: string;
}
export interface AnalyticsSummary {
  total_agents: number; running_agents: number; failed_agents: number;
  total_executions: number; active_executions: number;
  total_logs: number; alerts: AlertCounts;
}
export interface MetricHistory {
  agent_id: string; metric_name: string; metric_value: number; timestamp: string;
}

export const api = {
  agents: {
    list:       ()               => request<Agent[]>('/api/v1/agents'),
    get:        (id: string)     => request<Agent>(`/api/v1/agents/${id}`),
    create:     (data: Partial<Agent> & { env_vars?: Record<string, string> }) =>
                  request<Agent>('/api/v1/agents', { method: 'POST', body: JSON.stringify(data) }),
    update:     (id: string, data: Partial<Agent>) =>
                  request<Agent>(`/api/v1/agents/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete:     (id: string) => request<void>(`/api/v1/agents/${id}`, { method: 'DELETE' }),
    start:      (id: string) => request<{ success: boolean }>(`/api/v1/agents/${id}/start`, { method: 'POST' }),
    stop:       (id: string) => request<{ success: boolean }>(`/api/v1/agents/${id}/stop`, { method: 'POST' }),
    restart:    (id: string) => request<{ success: boolean }>(`/api/v1/agents/${id}/restart`, { method: 'POST' }),
    executions: (id: string)      => request<Execution[]>(`/api/v1/agents/${id}/executions`),
    logs:       (id: string, limit = 200) => request<Log[]>(`/api/v1/agents/${id}/logs?limit=${limit}`),
    events:     (id: string)      => request<Event[]>(`/api/v1/agents/${id}/events`),
    metrics:    (id: string, name?: string) =>
                  request<Metric[]>(`/api/v1/agents/${id}/metrics${name ? `?metric_name=${name}` : ''}`),
  },
  system: {
    health: () => request<SystemHealth>('/api/v1/system/health'),
  },
  events: {
    list:    (limit = 100)   => request<Event[]>(`/api/v1/events?limit=${limit}`),
  },
  metrics: {
    list:    (limit = 100)   => request<Metric[]>(`/api/v1/metrics?limit=${limit}`),
    history: (params: { agent_id?: string; metric_name?: string; range?: string; limit?: number }) => {
      const q = new URLSearchParams();
      if (params.agent_id)    q.set('agent_id', params.agent_id);
      if (params.metric_name) q.set('metric_name', params.metric_name);
      if (params.range)       q.set('range', params.range);
      if (params.limit)       q.set('limit', String(params.limit));
      return request<MetricHistory[]>(`/api/v1/analytics/metrics/history?${q}`);
    },
  },
  logs: {
    list: (limit = 100) => request<Log[]>(`/api/v1/logs?limit=${limit}`),
  },
  alerts: {
  list: (resolved?: boolean) => {
    const params = new URLSearchParams();
    if (resolved !== undefined) params.set('resolved', String(resolved));
    return request<Alert[]>(`/api/v1/alerts?${params}`);
  },
  counts:  () => request<AlertCounts>('/api/v1/alerts/counts'),
  resolve: (id: string) => request<Alert>(`/api/v1/alerts/${id}/resolve`, { method: 'POST' }),
},
  analytics: {
    summary:       () => request<AnalyticsSummary>('/api/v1/analytics/summary'),
    eventsHistory: (range = '1h') => request<Event[]>(`/api/v1/analytics/events/history?range=${range}`),
  },
};