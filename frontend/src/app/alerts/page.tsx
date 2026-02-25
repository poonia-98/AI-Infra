'use client';
import { useEffect, useState, useCallback } from 'react';
import { AlertTriangle, Bell, CheckCircle, Clock, RefreshCw, Filter, X, Cpu, HardDrive, Bot, Zap } from 'lucide-react';
import { api, Alert, AlertCounts } from '@/lib/api';
import { format, formatDistanceToNow } from 'date-fns';
import { useWebSocket } from '@/hooks/useWebSocket';
import clsx from 'clsx';

const SEV_CFG: Record<string, { color: string; bg: string; border: string; icon: typeof AlertTriangle }> = {
  critical: { color: 'var(--crimson)', bg: 'rgba(255,61,85,0.08)',  border: 'rgba(255,61,85,0.3)',  icon: AlertTriangle },
  warning:  { color: 'var(--amber)',   bg: 'rgba(255,184,0,0.08)',  border: 'rgba(255,184,0,0.25)', icon: AlertTriangle },
  info:     { color: 'var(--cyan)',    bg: 'rgba(0,217,255,0.08)',  border: 'rgba(0,217,255,0.25)', icon: Bell },
};

const TYPE_ICON: Record<string, string> = {
  high_cpu:        '⚡ HIGH CPU',
  critical_cpu:    '🔥 CRITICAL CPU',
  high_memory:     '💾 HIGH MEMORY',
  agent_failure:   '🤖 AGENT FAILED',
  execution_failure: '⚠ EXEC FAILED',
  container_crash: '💀 CONTAINER CRASH',
};

type StatusFilter = 'all' | 'active' | 'resolved';

export default function AlertsPage() {
  const [alerts, setAlerts]       = useState<Alert[]>([]);
  const [counts, setCounts]       = useState<AlertCounts>({ active: 0, critical: 0, warnings: 0, resolved: 0 });
  const [filter, setFilter]       = useState<StatusFilter>('active');
  const [sevFilter, setSevFilter] = useState<string>('all');
  const [loading, setLoading]     = useState(true);
  const [busy, setBusy]           = useState<Record<string, boolean>>({});
  const { subscribe }             = useWebSocket();

  const fetchAlerts = useCallback(async () => {
    try {
      const [a, c] = await Promise.all([
        api.alerts.list(filter === 'all' ? undefined : filter),
        api.alerts.counts(),
      ]);
      setAlerts(a); setCounts(c);
    } catch {} finally { setLoading(false); }
  }, [filter]);

  useEffect(() => { fetchAlerts(); }, [fetchAlerts]);
  useEffect(() => {
    const t = setInterval(fetchAlerts, 15000);
    return () => clearInterval(t);
  }, [fetchAlerts]);

  useEffect(() => {
    const unsub = subscribe('alerts.stream', () => { setTimeout(fetchAlerts, 300); });
    return unsub;
  }, [subscribe, fetchAlerts]);

  async function resolveAlert(id: string) {
    setBusy(b => ({ ...b, [id]: true }));
    try {
      await api.alerts.resolve(id);
      await fetchAlerts();
    } catch (e) { alert(e instanceof Error ? e.message : 'Failed'); }
    finally { setBusy(b => { const n = {...b}; delete n[id]; return n; }); }
  }

  const filtered = alerts.filter(a => sevFilter === 'all' || a.severity === sevFilter);

  return (
    <div style={{ height: '100%', overflowY: 'auto', background: 'var(--base)' }}>
      {/* Header */}
      <div style={{ position: 'sticky', top: 0, zIndex: 40, background: 'rgba(3,7,15,0.97)', backdropFilter: 'blur(20px)', borderBottom: '1px solid var(--line-0)' }}>
        <div style={{ height: 1, background: 'linear-gradient(90deg,var(--crimson),var(--amber))', opacity: 0.4 }} />
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '7px 16px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <Bell style={{ width: 12, height: 12, color: counts.active > 0 ? 'var(--crimson)' : 'var(--ink-3)' }} />
            <span style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.1em' }}>ALERT ENGINE</span>
            {counts.active > 0 && (
              <span style={{ padding: '2px 7px', background: 'rgba(255,61,85,0.12)', border: '1px solid rgba(255,61,85,0.3)', color: 'var(--crimson)', fontFamily: 'var(--mono)', fontSize: 9, fontWeight: 700 }}>
                {counts.active} ACTIVE
              </span>
            )}
          </div>
          <button onClick={fetchAlerts} className="op-btn op-btn-ghost" style={{ padding: '4px 10px', fontSize: 9 }}>
            <RefreshCw style={{ width: 9, height: 9 }} /> REFRESH
          </button>
        </div>
      </div>

      <div style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>
        {/* Counts strip */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 8 }}>
          {[
            { label: 'ACTIVE',   val: counts.active,   color: counts.active > 0 ? 'var(--crimson)' : 'var(--ink-1)', accent: 'kpi-crimson' },
            { label: 'CRITICAL', val: counts.critical,  color: counts.critical > 0 ? 'var(--crimson)' : 'var(--ink-1)', accent: 'kpi-crimson' },
            { label: 'WARNINGS', val: counts.warnings,  color: counts.warnings > 0 ? 'var(--amber)'   : 'var(--ink-1)', accent: 'kpi-amber' },
            { label: 'RESOLVED', val: counts.resolved,  color: 'var(--lime)',                                             accent: 'kpi-lime' },
          ].map(({ label, val, color, accent }) => (
            <div key={label} className={`kpi-card ${accent} ${val > 0 && label !== 'RESOLVED' ? 'kpi-active' : ''}`}>
              <div style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', fontWeight: 700, marginBottom: 4 }}>{label}</div>
              <div style={{ fontFamily: 'var(--mono)', fontSize: '2rem', fontWeight: 700, color, fontVariantNumeric: 'tabular-nums' }}>{val}</div>
            </div>
          ))}
        </div>

        {/* Rules reference */}
        <div className="panel">
          <div className="panel-header">
            <Filter style={{ width: 10, height: 10, color: 'var(--amber)' }} />
            <span>ACTIVE ALERT RULES</span>
          </div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 6, padding: 10 }}>
            {[
              { type: 'high_cpu',     sev: 'warning',  metric: 'cpu_percent',  threshold: '> 80%',  icon: Cpu },
              { type: 'critical_cpu', sev: 'critical', metric: 'cpu_percent',  threshold: '> 95%',  icon: Cpu },
              { type: 'high_memory',  sev: 'warning',  metric: 'memory_mb',    threshold: '> 400MB', icon: HardDrive },
              { type: 'agent_failure',sev: 'critical', metric: 'agent.failed', threshold: 'ANY',    icon: Bot },
            ].map(r => {
              const cfg = SEV_CFG[r.sev] ?? SEV_CFG.warning;
              const Icon = r.icon;
              return (
                <div key={r.type} style={{ padding: '8px 10px', background: cfg.bg, border: `1px solid ${cfg.border}`, display: 'flex', alignItems: 'center', gap: 8 }}>
                  <Icon style={{ width: 12, height: 12, color: cfg.color, flexShrink: 0 }} />
                  <div>
                    <div style={{ fontFamily: 'var(--mono)', fontSize: 9, fontWeight: 700, color: cfg.color, letterSpacing: '0.08em' }}>
                      {r.type.toUpperCase().replace(/_/g,' ')}
                    </div>
                    <div style={{ fontSize: 8.5, color: 'var(--ink-3)', marginTop: 1 }}>
                      {r.metric} {r.threshold} · <span style={{ color: r.sev === 'critical' ? 'var(--crimson)' : 'var(--amber)' }}>{r.sev}</span>
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        {/* Alert list */}
        <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
          {/* Filter tabs */}
          <div style={{ display: 'flex', alignItems: 'center', gap: 0, borderBottom: '1px solid var(--line-0)', background: 'var(--surface-0)' }}>
            {(['all', 'active', 'resolved'] as StatusFilter[]).map(f => (
              <button key={f} onClick={() => setFilter(f)} style={{
                padding: '7px 16px', background: filter === f ? 'rgba(0,217,255,0.06)' : 'transparent',
                border: 'none', borderBottom: filter === f ? '2px solid var(--cyan)' : '2px solid transparent',
                color: filter === f ? 'var(--cyan)' : 'var(--ink-3)',
                fontFamily: 'var(--mono)', fontSize: 9, fontWeight: 700, letterSpacing: '0.12em',
                cursor: 'pointer', transition: 'all 0.15s',
              }}>{f.toUpperCase()}</button>
            ))}
            <div style={{ marginLeft: 'auto', display: 'flex', gap: 4, padding: '4px 12px', borderLeft: '1px solid var(--line-0)' }}>
              <span style={{ fontSize: 8.5, color: 'var(--ink-3)', alignSelf: 'center' }}>SEVERITY:</span>
              {['all', 'critical', 'warning'].map(s => (
                <button key={s} onClick={() => setSevFilter(s)} style={{
                  padding: '2px 7px', fontFamily: 'var(--mono)', fontSize: 8, fontWeight: 700,
                  background: sevFilter === s ? 'rgba(0,217,255,0.1)' : 'transparent',
                  border: `1px solid ${sevFilter === s ? 'rgba(0,217,255,0.3)' : 'var(--line-1)'}`,
                  color: sevFilter === s ? 'var(--cyan)' : 'var(--ink-3)', cursor: 'pointer',
                }}>{s.toUpperCase()}</button>
              ))}
            </div>
          </div>

          {loading ? (
            <div style={{ padding: 40, textAlign: 'center', color: 'var(--ink-3)', fontSize: 10 }}>LOADING ALERTS...</div>
          ) : filtered.length === 0 ? (
            <div style={{ padding: 48, textAlign: 'center', display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 12 }}>
              <CheckCircle style={{ width: 28, height: 28, color: 'var(--lime)' }} />
              <div style={{ color: 'var(--ink-2)', fontSize: 11, letterSpacing: '0.1em' }}>
                {filter === 'active' ? 'ALL SYSTEMS HEALTHY — NO ACTIVE ALERTS' : 'NO ALERTS IN THIS RANGE'}
              </div>
              <div style={{ fontSize: 9, color: 'var(--ink-3)' }}>Alert rules are monitoring CPU, memory, and agent health continuously</div>
            </div>
          ) : (
            <div>
              {filtered.map(alert => {
                const cfg = SEV_CFG[alert.severity] ?? SEV_CFG.warning;
                const Icon = cfg.icon;
                return (
                  <div key={alert.id} style={{
                    padding: '10px 14px', borderBottom: '1px solid var(--line-0)',
                    background: alert.status === 'active' ? cfg.bg : 'transparent',
                    borderLeft: `3px solid ${alert.status === 'active' ? cfg.color : 'var(--line-1)'}`,
                    display: 'flex', alignItems: 'flex-start', gap: 12,
                    opacity: alert.status === 'resolved' ? 0.6 : 1,
                    transition: 'all 0.2s',
                  }}>
                    <Icon style={{ width: 14, height: 14, color: cfg.color, flexShrink: 0, marginTop: 1 }} />
                    <div style={{ flex: 1, minWidth: 0 }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 3 }}>
                        <span style={{ fontFamily: 'var(--mono)', fontSize: 10, fontWeight: 700, color: alert.status === 'active' ? cfg.color : 'var(--ink-2)' }}>
                          {(TYPE_ICON[alert.alert_type] || alert.alert_type.toUpperCase().replace(/_/g,' '))}
                        </span>
                        <span style={{ fontSize: 8.5, padding: '1px 5px', background: alert.status === 'active' ? cfg.bg : 'var(--surface-1)', border: `1px solid ${alert.status === 'active' ? cfg.border : 'var(--line-1)'}`, color: alert.status === 'active' ? cfg.color : 'var(--ink-3)', fontFamily: 'var(--mono)', fontWeight: 700 }}>
                          {alert.severity.toUpperCase()}
                        </span>
                        {alert.status === 'resolved' && (
                          <span style={{ fontSize: 8.5, padding: '1px 5px', background: 'rgba(57,255,20,0.06)', border: '1px solid rgba(57,255,20,0.2)', color: 'var(--lime)', fontFamily: 'var(--mono)', fontWeight: 700 }}>RESOLVED</span>
                        )}
                      </div>
                      <div style={{ fontSize: 10.5, color: 'var(--ink-1)', marginBottom: 4 }}>{alert.message}</div>
                      <div style={{ display: 'flex', gap: 12, fontSize: 8.5, color: 'var(--ink-3)', fontFamily: 'var(--mono)' }}>
                        {alert.agent_id && <span>AGENT: {alert.agent_id.slice(0,8)}</span>}
                        {alert.value !== null && <span>VALUE: {alert.value?.toFixed(1)}</span>}
                        {alert.threshold !== null && <span>THRESHOLD: {alert.threshold}</span>}
                        <span style={{ display: 'flex', alignItems: 'center', gap: 3 }}>
                          <Clock style={{ width: 8, height: 8 }} />
                          {formatDistanceToNow(new Date(alert.created_at), { addSuffix: true })}
                        </span>
                        {alert.resolved_at && <span style={{ color: 'var(--lime)' }}>RESOLVED: {formatDistanceToNow(new Date(alert.resolved_at), { addSuffix: true })}</span>}
                      </div>
                    </div>
                    {alert.status === 'active' && (
                      <button
                        onClick={() => resolveAlert(alert.id)}
                        disabled={busy[alert.id]}
                        className="op-btn op-btn-ghost"
                        style={{ padding: '3px 9px', fontSize: 8, flexShrink: 0 }}
                      >
                        {busy[alert.id] ? '...' : <><CheckCircle style={{ width: 9, height: 9 }} /> RESOLVE</>}
                      </button>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}