'use client';

import { useEffect, useState, useCallback } from 'react';
import { AlertTriangle, Bell, CheckCircle, Clock, RefreshCw, Filter, X, Cpu, HardDrive, Bot, Zap } from 'lucide-react';
import { api, Alert, AlertCounts } from '@/lib/api';

const SEVERITY_META: Record<string, { color: string; bg: string; label: string }> = {
  critical: { color: 'var(--crimson)', bg: 'rgba(255,59,48,0.08)',  label: 'CRITICAL' },
  warning:  { color: 'var(--amber)',   bg: 'rgba(255,159,10,0.08)', label: 'WARNING'  },
  info:     { color: 'var(--cyan)',    bg: 'rgba(0,217,255,0.08)',  label: 'INFO'     },
};

const METRIC_ICON: Record<string, typeof Cpu> = {
  cpu:    Cpu,
  memory: HardDrive,
  agent:  Bot,
  default: Zap,
};

function getIcon(metricName: string) {
  const key = Object.keys(METRIC_ICON).find(k => metricName.toLowerCase().includes(k)) || 'default';
  return METRIC_ICON[key];
}

function formatTime(ts: string) {
  const diff = (Date.now() - new Date(ts).getTime()) / 1000;
  if (diff < 60)  return `${Math.floor(diff)}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  return `${Math.floor(diff / 3600)}h ago`;
}

export default function AlertsPage() {
  const [alerts, setAlerts]         = useState<Alert[]>([]);
  const [counts, setCounts]         = useState<AlertCounts | null>(null);
  const [loading, setLoading]       = useState(true);
  const [filter, setFilter]         = useState<'all' | 'active' | 'resolved'>('active');
  const [severity, setSeverity]     = useState<string>('all');
  const [resolving, setResolving]   = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const resolved = filter === 'all' ? undefined : filter === 'resolved';
      const [data, cnt] = await Promise.allSettled([
        api.alerts.list(resolved, 200),
        api.alerts.counts(),
      ]);
      if (data.status === 'fulfilled') setAlerts(data.value);
      if (cnt.status === 'fulfilled')  setCounts(cnt.value);
    } catch {
      // silent — data stays as-is
    } finally {
      setLoading(false);
    }
  }, [filter]);

  useEffect(() => { load(); }, [load]);

  const handleResolve = async (id: string) => {
    setResolving(id);
    try {
      await api.alerts.resolve(id);
      setAlerts(prev => prev.map(a => a.id === id ? { ...a, resolved: true, resolved_at: new Date().toISOString() } : a));
    } finally {
      setResolving(null);
    }
  };

  const visible = alerts.filter(a => severity === 'all' || a.severity === severity);

  return (
    <div style={{ padding: '28px 32px', fontFamily: 'var(--mono)', maxWidth: 1100 }}>

      {/* Header */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 24 }}>
        <div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
            <Bell style={{ width: 16, height: 16, color: 'var(--amber)' }} />
            <h1 style={{ fontSize: 14, fontWeight: 700, letterSpacing: '0.12em', color: 'var(--ink-0)', margin: 0 }}>
              ALERTS
            </h1>
          </div>
          <p style={{ fontSize: 10, color: 'var(--ink-4)', margin: 0 }}>
            {counts?.active ?? '--'} active · {counts?.resolved ?? '--'} resolved
          </p>
        </div>
        <button
          onClick={load}
          style={{ background: 'transparent', border: '1px solid var(--line-1)', color: 'var(--ink-3)', cursor: 'pointer', padding: '6px 12px', fontSize: 9, letterSpacing: '0.1em', fontFamily: 'var(--mono)', display: 'flex', alignItems: 'center', gap: 6 }}
        >
          <RefreshCw style={{ width: 9, height: 9 }} /> REFRESH
        </button>
      </div>

      {/* Counts row */}
      {counts && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12, marginBottom: 24 }}>
          {[
            { label: 'TOTAL',    value: counts.total,    color: 'var(--ink-2)' },
            { label: 'ACTIVE',   value: counts.active,   color: 'var(--crimson)' },
            { label: 'CRITICAL', value: counts.critical, color: 'var(--crimson)' },
            { label: 'WARNING',  value: counts.warning,  color: 'var(--amber)' },
          ].map(card => (
            <div key={card.label} style={{ background: 'var(--surface-1)', border: '1px solid var(--line-1)', padding: '14px 16px' }}>
              <div style={{ fontSize: 20, fontWeight: 700, color: card.color }}>{card.value}</div>
              <div style={{ fontSize: 8, color: 'var(--ink-4)', letterSpacing: '0.14em', marginTop: 2 }}>{card.label}</div>
            </div>
          ))}
        </div>
      )}

      {/* Filters */}
      <div style={{ display: 'flex', gap: 8, marginBottom: 20 }}>
        {(['all', 'active', 'resolved'] as const).map(f => (
          <button key={f} onClick={() => setFilter(f)} style={{
            background: filter === f ? 'var(--cyan)' : 'transparent',
            color: filter === f ? 'var(--surface-0)' : 'var(--ink-3)',
            border: `1px solid ${filter === f ? 'var(--cyan)' : 'var(--line-1)'}`,
            padding: '4px 12px', fontSize: 9, letterSpacing: '0.1em',
            cursor: 'pointer', fontFamily: 'var(--mono)',
          }}>
            {f.toUpperCase()}
          </button>
        ))}
        <div style={{ flex: 1 }} />
        <Filter style={{ width: 10, height: 10, color: 'var(--ink-4)', alignSelf: 'center' }} />
        {(['all', 'critical', 'warning', 'info'] as const).map(s => (
          <button key={s} onClick={() => setSeverity(s)} style={{
            background: severity === s ? 'rgba(255,255,255,0.06)' : 'transparent',
            color: s === 'all' ? 'var(--ink-2)' : (SEVERITY_META[s]?.color ?? 'var(--ink-2)'),
            border: `1px solid ${severity === s ? 'var(--line-2)' : 'var(--line-0)'}`,
            padding: '4px 10px', fontSize: 9, letterSpacing: '0.1em',
            cursor: 'pointer', fontFamily: 'var(--mono)',
          }}>
            {s.toUpperCase()}
          </button>
        ))}
      </div>

      {/* Alert list */}
      {loading ? (
        <div style={{ textAlign: 'center', padding: 40, color: 'var(--ink-4)', fontSize: 10 }}>Loading alerts...</div>
      ) : visible.length === 0 ? (
        <div style={{ textAlign: 'center', padding: 40 }}>
          <CheckCircle style={{ width: 28, height: 28, color: 'var(--lime)', margin: '0 auto 10px' }} />
          <div style={{ fontSize: 11, color: 'var(--ink-3)' }}>No alerts match the current filter</div>
        </div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {visible.map(alert => {
            const meta = SEVERITY_META[alert.severity] ?? SEVERITY_META.info;
            const Icon = getIcon(alert.metric_name);
            return (
              <div key={alert.id} style={{
                background: 'var(--surface-1)',
                border: `1px solid var(--line-1)`,
                borderLeft: `3px solid ${meta.color}`,
                padding: '12px 16px',
                display: 'flex', alignItems: 'center', gap: 14,
                opacity: alert.resolved ? 0.55 : 1,
              }}>
                <Icon style={{ width: 13, height: 13, color: meta.color, flexShrink: 0 }} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 3 }}>
                    <span style={{ fontSize: 9, fontWeight: 700, color: meta.color, letterSpacing: '0.1em', background: meta.bg, padding: '1px 6px' }}>
                      {meta.label}
                    </span>
                    <span style={{ fontSize: 10, color: 'var(--ink-1)', fontWeight: 600 }}>
                      {alert.metric_name}
                    </span>
                    {alert.agent_id && (
                      <span style={{ fontSize: 8, color: 'var(--ink-4)' }}>
                        agent:{alert.agent_id.slice(0, 8)}
                      </span>
                    )}
                  </div>
                  <div style={{ fontSize: 10, color: 'var(--ink-3)' }}>{alert.message}</div>
                  <div style={{ fontSize: 8, color: 'var(--ink-4)', marginTop: 4, display: 'flex', gap: 16 }}>
                    <span>value: {alert.metric_value}</span>
                    <span>threshold: {alert.threshold}</span>
                    <Clock style={{ width: 8, height: 8, display: 'inline' }} />
                    <span>{formatTime(alert.created_at)}</span>
                  </div>
                </div>
                {!alert.resolved ? (
                  <button
                    onClick={() => handleResolve(alert.id)}
                    disabled={resolving === alert.id}
                    style={{
                      background: 'transparent', border: '1px solid var(--lime)',
                      color: 'var(--lime)', cursor: 'pointer', padding: '4px 10px',
                      fontSize: 8, letterSpacing: '0.1em', fontFamily: 'var(--mono)',
                      opacity: resolving === alert.id ? 0.5 : 1,
                      flexShrink: 0,
                    }}
                  >
                    RESOLVE
                  </button>
                ) : (
                  <div style={{ display: 'flex', alignItems: 'center', gap: 4, color: 'var(--lime)', fontSize: 8, flexShrink: 0 }}>
                    <CheckCircle style={{ width: 9, height: 9 }} /> RESOLVED
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}