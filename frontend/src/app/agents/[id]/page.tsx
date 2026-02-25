'use client';
import { useEffect, useState, useCallback, useRef } from 'react';
import { useParams, useRouter } from 'next/navigation';
import Link from 'next/link';
import {
  ArrowLeft, Play, Square, RotateCcw, Trash2, Cpu, HardDrive,
  Terminal, Activity, Clock, AlertTriangle, CheckCircle,
  ChevronRight, Server, Zap, BarChart2,
} from 'lucide-react';
import { api, Agent, Log, Execution, Metric } from '@/lib/api';
import { useWebSocket } from '@/hooks/useWebSocket';
import { format, formatDistanceToNow, formatDuration, intervalToDuration } from 'date-fns';
import {
  AreaChart, Area, LineChart, Line, XAxis, YAxis,
  CartesianGrid, Tooltip, ResponsiveContainer, BarChart, Bar,
} from 'recharts';
import clsx from 'clsx';

const STATUS_TAG: Record<string, string> = {
  running: 'tag-running', stopped: 'tag-stopped', created: 'tag-created',
  error: 'tag-error', failed: 'tag-failed', idle: 'tag-idle',
  pending: 'tag-pending', completed: 'tag-completed',
};
const EXEC_TAG: Record<string, string> = {
  running: 'tag-running', completed: 'tag-completed',
  failed: 'tag-error', pending: 'tag-pending', error: 'tag-error',
};

const LOG_CFG: Record<string, { color: string; bg: string; border: string }> = {
  error:   { color: 'var(--crimson)', bg: 'rgba(255,61,85,0.14)',  border: 'rgba(255,61,85,0.35)' },
  warn:    { color: 'var(--amber)',   bg: 'rgba(255,184,0,0.12)',  border: 'rgba(255,184,0,0.35)' },
  warning: { color: 'var(--amber)',   bg: 'rgba(255,184,0,0.12)',  border: 'rgba(255,184,0,0.35)' },
  info:    { color: 'var(--cyan)',    bg: 'rgba(0,217,255,0.10)',  border: 'rgba(0,217,255,0.3)' },
  debug:   { color: 'var(--ink-3)',   bg: 'transparent',           border: 'transparent' },
};

const TT = {
  contentStyle: { background: '#010306', border: '1px solid #102036', fontSize: 10, fontFamily: '"JetBrains Mono",monospace', color: '#7ba8c8', padding: '6px 10px', borderRadius: 0 },
  labelStyle:   { color: '#1e3d54' },
  cursor:       { stroke: 'rgba(0,217,255,0.12)' },
};

interface ChartPt { label: string; cpu: number; mem: number; }

function Beacon({ state }: { state: 'online'|'active'|'warn'|'error'|'dead' }) {
  return (
    <div className="beacon" style={{ width: 12, height: 12, flexShrink: 0 }}>
      <div className={`beacon-core beacon-${state}`} style={{ width: 6, height: 6 }} />
      {(state === 'online' || state === 'active') && (
        <div className={`beacon-ring beacon-${state}`} style={{ position: 'absolute', inset: 0, borderRadius: '50%' }} />
      )}
    </div>
  );
}

export default function AgentDetailPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const [agent, setAgent]           = useState<Agent | null>(null);
  const [logs, setLogs]             = useState<Log[]>([]);
  const [executions, setExecutions] = useState<Execution[]>([]);
  const [metrics, setMetrics]       = useState<Metric[]>([]);
  const [chart, setChart]           = useState<ChartPt[]>([]);
  const [tab, setTab]               = useState<'logs'|'executions'|'metrics'>('logs');
  const [loading, setLoading]       = useState(true);
  const [busy, setBusy]             = useState<string | null>(null);
  const [liveCpu, setLiveCpu]       = useState(0);
  const [liveMem, setLiveMem]       = useState(0);
  const logEndRef   = useRef<HTMLDivElement>(null);
  const logScrollRef= useRef<HTMLDivElement>(null);
  const [pinLogs, setPinLogs]       = useState(true);
  const [tick, setTick]             = useState(0);
  const { subscribe } = useWebSocket(id as string);

  useEffect(() => {
    const t = setInterval(() => setTick(n => n + 1), 5000);
    return () => clearInterval(t);
  }, []);

  useEffect(() => {
    if (tick % 2 === 0) {
      const label = format(new Date(), 'HH:mm:ss');
      setChart(prev => [...prev, { label, cpu: liveCpu, mem: liveMem }].slice(-60));
    }
  }, [tick, liveCpu, liveMem]);

  const fetchAll = useCallback(async () => {
    if (!id) return;
    try {
      const [a, l, e, m] = await Promise.all([
        api.agents.get(id), api.agents.logs(id, 200),
        api.agents.executions(id), api.agents.metrics(id),
      ]);
      setAgent(a); setLogs(l); setExecutions(e); setMetrics(m);
    } catch {} finally { setLoading(false); }
  }, [id]);

  useEffect(() => {
    fetchAll();
    const t = setInterval(fetchAll, 15000);
    return () => clearInterval(t);
  }, [fetchAll]);

  useEffect(() => {
    const unsubLog = subscribe('logs.stream', msg => {
      const d = msg.data as { agent_id?: string; message?: string; level?: string; source?: string; timestamp?: string };
      if (d.agent_id !== id) return;
      setLogs(prev => [{ id: crypto.randomUUID(), agent_id: d.agent_id!, execution_id: null,
        level: d.level || 'info', message: d.message || '', source: d.source || null,
        metadata: {}, timestamp: d.timestamp || new Date().toISOString() }, ...prev].slice(0, 500));
    });
    const unsubMet = subscribe('metrics.stream', msg => {
      const d = msg.data as { agent_id?: string; metric_name?: string; metric_value?: number; timestamp?: string };
      if (d.agent_id !== id) return;
      if (d.metric_name === 'cpu_percent') setLiveCpu(d.metric_value || 0);
      if (d.metric_name === 'memory_mb')   setLiveMem(d.metric_value || 0);
      setMetrics(prev => [{ id: crypto.randomUUID(), agent_id: d.agent_id!, metric_name: d.metric_name || '',
        metric_value: d.metric_value || 0, labels: {}, timestamp: d.timestamp || new Date().toISOString() },
        ...prev].slice(0, 500));
    });
    const unsubEv = subscribe('agents.events', msg => {
      const d = msg.data as { agent_id?: string; event_type?: string };
      if (d.agent_id !== id) return;
      if (['agent.started','agent.stopped','agent.failed','agent.completed'].includes(d.event_type || '')) {
        setTimeout(() => { api.agents.get(id).then(a => setAgent(a)).catch(() => {}); }, 500);
      }
    });
    return () => { unsubLog(); unsubMet(); unsubEv(); };
  }, [subscribe, id]);

  useEffect(() => {
    if (pinLogs) logEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [logs, pinLogs]);

  async function act(action: 'start'|'stop'|'restart'|'delete') {
    if (!id) return;
    setBusy(action);
    try {
      if (action === 'delete') { await api.agents.delete(id); router.push('/agents'); return; }
      await api.agents[action](id);
      await fetchAll();
    } catch (e) { alert(e instanceof Error ? e.message : 'Action failed'); }
    finally { setBusy(null); }
  }

  const isRunning = agent?.status === 'running';
  const cpuColor  = liveCpu > 80 ? 'var(--crimson)' : liveCpu > 60 ? 'var(--amber)' : 'var(--cyan)';
  const memPct    = Math.min(100, (liveMem / 512) * 100);
  const memColor  = memPct > 80 ? 'var(--crimson)' : memPct > 60 ? 'var(--amber)' : 'var(--lime)';

  const cpuMetrics = metrics.filter(m => m.metric_name === 'cpu_percent').slice(0, 60);
  const memMetrics = metrics.filter(m => m.metric_name === 'memory_mb').slice(0, 60);

  if (loading) return (
    <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 11 }}>
      LOADING AGENT DATA...
    </div>
  );
  if (!agent) return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 12 }}>
      <div style={{ color: 'var(--crimson)', fontSize: 11 }}>AGENT NOT FOUND</div>
      <Link href="/agents" className="op-btn op-btn-ghost" style={{ fontSize: 9 }}>← BACK TO FLEET</Link>
    </div>
  );

  return (
    <div style={{ height: '100%', overflowY: 'auto', background: 'var(--base)' }}>
      {/* Header */}
      <div style={{ position: 'sticky', top: 0, zIndex: 40, background: 'rgba(3,7,15,0.97)', backdropFilter: 'blur(20px)', borderBottom: '1px solid var(--line-0)' }}>
        <div style={{ height: 1, background: 'linear-gradient(90deg,var(--cyan),var(--violet))', opacity: 0.4 }} />
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '7px 16px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <Link href="/agents" style={{ color: 'var(--ink-3)', display: 'flex', alignItems: 'center', gap: 4, textDecoration: 'none', fontSize: 10 }}>
              <ArrowLeft style={{ width: 11, height: 11 }} /> FLEET
            </Link>
            <ChevronRight style={{ width: 10, height: 10, color: 'var(--ink-4)' }} />
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              {isRunning && <Beacon state="online" />}
              <span style={{ fontFamily: 'var(--display)', fontSize: 15, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>
                {agent.name.toUpperCase()}
              </span>
              <span className={`tag ${STATUS_TAG[agent.status] ?? 'tag-stopped'}`}>{agent.status}</span>
            </div>
            <span style={{ fontSize: 9, color: 'var(--ink-3)', fontFamily: 'var(--mono)' }}>{agent.id.slice(0, 8)}</span>
          </div>
          <div style={{ display: 'flex', gap: 6 }}>
            {!isRunning && <button className="op-btn op-btn-lime" style={{ padding: '4px 10px', fontSize: 9 }} disabled={!!busy} onClick={() => act('start')}><Play style={{ width: 9, height: 9 }} />{busy === 'start' ? '...' : 'START'}</button>}
            {isRunning && <button className="op-btn op-btn-crimson" style={{ padding: '4px 10px', fontSize: 9 }} disabled={!!busy} onClick={() => act('stop')}><Square style={{ width: 9, height: 9 }} />{busy === 'stop' ? '...' : 'STOP'}</button>}
            {isRunning && <button className="op-btn op-btn-cyan" style={{ padding: '4px 10px', fontSize: 9 }} disabled={!!busy} onClick={() => act('restart')}><RotateCcw style={{ width: 9, height: 9 }} />{busy === 'restart' ? '...' : 'RESTART'}</button>}
            <button className="op-btn op-btn-ghost" style={{ padding: '4px 10px', fontSize: 9 }} disabled={!!busy || isRunning} onClick={() => { if (confirm('Delete this agent?')) act('delete'); }}><Trash2 style={{ width: 9, height: 9 }} /></button>
          </div>
        </div>
      </div>

      <div style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>
        {/* Top info row */}
        <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr 1fr 1fr', gap: 8 }}>
          {/* Agent info */}
          <div className="panel" style={{ padding: '12px 14px' }}>
            <div style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', fontWeight: 700, marginBottom: 10 }}>AGENT CONFIGURATION</div>
            {[
              { k: 'ID',        v: agent.id.slice(0,8), mono: true },
              { k: 'TYPE',      v: agent.agent_type, mono: true },
              { k: 'IMAGE',     v: agent.image, mono: true },
              { k: 'CONTAINER', v: agent.container_id ? agent.container_id.slice(0,12) : '—', mono: true },
              { k: 'CREATED',   v: formatDistanceToNow(new Date(agent.created_at), { addSuffix: true }) },
              { k: 'UPDATED',   v: formatDistanceToNow(new Date(agent.updated_at), { addSuffix: true }) },
            ].map(({ k, v, mono }) => (
              <div key={k} style={{ display: 'flex', justifyContent: 'space-between', padding: '3px 0', borderBottom: '1px solid rgba(10,24,40,0.6)' }}>
                <span style={{ fontSize: 9, color: 'var(--ink-3)' }}>{k}</span>
                <span style={{ fontSize: 9, color: 'var(--ink-1)', fontFamily: mono ? 'var(--mono)' : undefined, maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v}</span>
              </div>
            ))}
          </div>

          {/* Live CPU */}
          <div className="panel" style={{ padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <Cpu style={{ width: 10, height: 10, color: 'var(--cyan)' }} />
              <span style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', fontWeight: 700 }}>CPU</span>
            </div>
            <div style={{ fontFamily: 'var(--mono)', fontSize: '2rem', fontWeight: 700, color: cpuColor, textShadow: `0 0 20px ${cpuColor}55`, fontVariantNumeric: 'tabular-nums', lineHeight: 1 }}>
              {liveCpu.toFixed(1)}<span style={{ fontSize: 12, color: 'var(--ink-3)', marginLeft: 2 }}>%</span>
            </div>
            <div className="gauge-track">
              <div className="gauge-fill" style={{ width: `${Math.min(100, liveCpu)}%`, background: cpuColor }} />
            </div>
            <div style={{ fontSize: 9, color: 'var(--ink-3)' }}>Limit: {agent.cpu_limit} cores</div>
          </div>

          {/* Live MEM */}
          <div className="panel" style={{ padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <HardDrive style={{ width: 10, height: 10, color: 'var(--lime)' }} />
              <span style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', fontWeight: 700 }}>MEMORY</span>
            </div>
            <div style={{ fontFamily: 'var(--mono)', fontSize: '2rem', fontWeight: 700, color: memColor, textShadow: `0 0 20px ${memColor}55`, fontVariantNumeric: 'tabular-nums', lineHeight: 1 }}>
              {liveMem.toFixed(0)}<span style={{ fontSize: 12, color: 'var(--ink-3)', marginLeft: 2 }}>MB</span>
            </div>
            <div className="gauge-track">
              <div className="gauge-fill" style={{ width: `${memPct}%`, background: memColor }} />
            </div>
            <div style={{ fontSize: 9, color: 'var(--ink-3)' }}>Limit: {agent.memory_limit}</div>
          </div>

          {/* Exec stats */}
          <div className="panel" style={{ padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: 6 }}>
            <div style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', fontWeight: 700, marginBottom: 4 }}>EXECUTIONS</div>
            {[
              { label: 'TOTAL',     val: executions.length,                                       color: 'var(--ink-1)' },
              { label: 'COMPLETE',  val: executions.filter(e => e.status === 'completed').length, color: 'var(--lime)' },
              { label: 'RUNNING',   val: executions.filter(e => e.status === 'running').length,   color: 'var(--cyan)' },
              { label: 'FAILED',    val: executions.filter(e => e.status === 'failed').length,    color: 'var(--crimson)' },
            ].map(({ label, val, color }) => (
              <div key={label} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '3px 0', borderBottom: '1px solid rgba(10,24,40,0.6)' }}>
                <span style={{ fontSize: 9, color: 'var(--ink-3)' }}>{label}</span>
                <span style={{ fontFamily: 'var(--mono)', fontSize: 13, fontWeight: 700, color, fontVariantNumeric: 'tabular-nums' }}>{val}</span>
              </div>
            ))}
          </div>
        </div>

        {/* Live charts */}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
          {/* CPU chart */}
          <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
            <div className="panel-header">
              <Cpu style={{ width: 10, height: 10, color: 'var(--cyan)' }} />
              <span style={{ letterSpacing: '0.14em' }}>CPU HISTORY</span>
              <span style={{ marginLeft: 'auto', color: 'var(--cyan)', fontSize: 12, fontWeight: 700, fontFamily: 'var(--mono)' }}>
                {liveCpu.toFixed(1)}%
              </span>
            </div>
            <div className="chart-area" style={{ padding: '8px 4px 4px', minHeight: 130 }}>
              {chart.length < 2 ? (
                <div style={{ height: 120, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 10 }}>▶ AWAITING DATA</div>
              ) : (
                <ResponsiveContainer width="100%" height={120}>
                  <AreaChart data={chart} margin={{ top: 4, right: 4, bottom: 0, left: -22 }}>
                    <defs><linearGradient id="gcpu" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="var(--cyan)" stopOpacity={0.4} /><stop offset="95%" stopColor="var(--cyan)" stopOpacity={0} /></linearGradient></defs>
                    <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,.95)" vertical={false} />
                    <XAxis dataKey="label" stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} interval="preserveStartEnd" />
                    <YAxis stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} width={28} />
                    <Tooltip {...TT} formatter={(v: number) => [`${v.toFixed(1)}%`, 'CPU']} />
                    <Area type="monotone" dataKey="cpu" stroke="var(--cyan)" strokeWidth={1.5} fill="url(#gcpu)" dot={false} isAnimationActive={false} activeDot={{ r: 3, fill: 'var(--cyan)', strokeWidth: 0 }} />
                  </AreaChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>

          {/* Memory chart */}
          <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
            <div className="panel-header">
              <HardDrive style={{ width: 10, height: 10, color: 'var(--lime)' }} />
              <span style={{ letterSpacing: '0.14em' }}>MEMORY HISTORY</span>
              <span style={{ marginLeft: 'auto', color: 'var(--lime)', fontSize: 12, fontWeight: 700, fontFamily: 'var(--mono)' }}>
                {liveMem.toFixed(0)}MB
              </span>
            </div>
            <div className="chart-area" style={{ padding: '8px 4px 4px', minHeight: 130 }}>
              {chart.length < 2 ? (
                <div style={{ height: 120, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 10 }}>▶ AWAITING DATA</div>
              ) : (
                <ResponsiveContainer width="100%" height={120}>
                  <AreaChart data={chart} margin={{ top: 4, right: 4, bottom: 0, left: -22 }}>
                    <defs><linearGradient id="gmem" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="var(--lime)" stopOpacity={0.4} /><stop offset="95%" stopColor="var(--lime)" stopOpacity={0} /></linearGradient></defs>
                    <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,.95)" vertical={false} />
                    <XAxis dataKey="label" stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} interval="preserveStartEnd" />
                    <YAxis stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} width={28} />
                    <Tooltip {...TT} formatter={(v: number) => [`${v.toFixed(0)}MB`, 'Memory']} />
                    <Area type="monotone" dataKey="mem" stroke="var(--lime)" strokeWidth={1.5} fill="url(#gmem)" dot={false} isAnimationActive={false} activeDot={{ r: 3, fill: 'var(--lime)', strokeWidth: 0 }} />
                  </AreaChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>
        </div>

        {/* Tab panel: logs / executions / metrics */}
        <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
          {/* Tabs */}
          <div style={{ display: 'flex', borderBottom: '1px solid var(--line-0)', background: 'var(--surface-0)' }}>
            {[
              { key: 'logs', label: 'LOG STREAM', icon: Terminal, count: logs.length },
              { key: 'executions', label: 'EXECUTIONS', icon: Activity, count: executions.length },
              { key: 'metrics', label: 'METRICS', icon: BarChart2, count: metrics.length },
            ].map(({ key, label, icon: Icon, count }) => (
              <button key={key} onClick={() => setTab(key as typeof tab)} style={{
                display: 'flex', alignItems: 'center', gap: 6, padding: '7px 14px',
                background: tab === key ? 'rgba(0,217,255,0.06)' : 'transparent',
                border: 'none', borderBottom: tab === key ? '2px solid var(--cyan)' : '2px solid transparent',
                color: tab === key ? 'var(--cyan)' : 'var(--ink-3)', cursor: 'pointer',
                fontFamily: 'var(--mono)', fontSize: 9, fontWeight: 700, letterSpacing: '0.12em',
                transition: 'all 0.15s',
              }}>
                <Icon style={{ width: 10, height: 10 }} />
                {label}
                <span style={{
                  fontSize: 8, padding: '1px 4px',
                  background: tab === key ? 'rgba(0,217,255,0.12)' : 'var(--surface-1)',
                  border: `1px solid ${tab === key ? 'rgba(0,217,255,0.2)' : 'var(--line-1)'}`,
                  color: tab === key ? 'var(--cyan)' : 'var(--ink-4)',
                }}>{count}</span>
              </button>
            ))}
          </div>

          {/* Log stream */}
          {tab === 'logs' && (
            <div className="terminal-view" style={{ height: 360 }}>
              <div className="scanline" />
              <div
                ref={logScrollRef}
                onScroll={e => { const el = e.currentTarget; setPinLogs(el.scrollHeight - el.scrollTop - el.clientHeight < 40); }}
                style={{ height: '100%', overflowY: 'auto', padding: '4px 0', position: 'relative', zIndex: 3 }}
              >
                {logs.length === 0 ? (
                  <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 10 }}>
                    <span style={{ color: 'var(--lime)' }}>$</span>&nbsp;AWAITING LOG STREAM&nbsp;<span className="anim-cursor" style={{ color: 'var(--lime)' }}>█</span>
                  </div>
                ) : [...logs].reverse().map(log => {
                  const cfg = LOG_CFG[log.level] ?? LOG_CFG.debug;
                  return (
                    <div key={log.id} className={clsx('log-entry', log.level === 'error' && 'lv-error', (log.level === 'warn'||log.level==='warning') && 'lv-warn')}>
                      <span className="log-ts">{format(new Date(log.timestamp), 'HH:mm:ss')}</span>
                      <span className="log-lvl" style={{ background: cfg.bg, border: `1px solid ${cfg.border}`, color: cfg.color }}>{log.level.slice(0,4).toUpperCase()}</span>
                      {log.source && <span className="log-src">[{log.source}]</span>}
                      <span className="log-msg">{log.message}</span>
                    </div>
                  );
                })}
                <div ref={logEndRef} />
              </div>
            </div>
          )}

          {/* Executions */}
          {tab === 'executions' && (
            <div style={{ overflowY: 'auto', maxHeight: 360 }}>
              {executions.length === 0 ? (
                <div style={{ padding: 32, textAlign: 'center', color: 'var(--ink-3)', fontSize: 10 }}>NO EXECUTIONS YET</div>
              ) : (
                <table className="op-table">
                  <thead>
                    <tr><th>#</th><th>ID</th><th>STATUS</th><th>STARTED</th><th>DURATION</th><th>EXIT CODE</th></tr>
                  </thead>
                  <tbody>
                    {executions.map((ex, i) => (
                      <tr key={ex.id}>
                        <td style={{ color: 'var(--ink-3)', fontFamily: 'var(--mono)', fontSize: 9 }}>{String(executions.length - i).padStart(2,'0')}</td>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--violet)' }}>{ex.id.slice(0,8)}</td>
                        <td><span className={`tag ${EXEC_TAG[ex.status] ?? 'tag-stopped'}`}>{ex.status}</span></td>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)' }}>
                          {ex.started_at ? format(new Date(ex.started_at), 'HH:mm:ss') : '—'}
                        </td>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-1)' }}>
                          {ex.duration_ms ? `${(ex.duration_ms/1000).toFixed(1)}s` : ex.status === 'running' ? <span style={{ color: 'var(--lime)' }}>RUNNING</span> : '—'}
                        </td>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: ex.exit_code === 0 ? 'var(--lime)' : ex.exit_code ? 'var(--crimson)' : 'var(--ink-3)' }}>
                          {ex.exit_code !== null ? ex.exit_code : '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          )}

          {/* Metrics */}
          {tab === 'metrics' && (
            <div style={{ overflowY: 'auto', maxHeight: 360 }}>
              {metrics.length === 0 ? (
                <div style={{ padding: 32, textAlign: 'center', color: 'var(--ink-3)', fontSize: 10 }}>NO METRICS YET</div>
              ) : (
                <table className="op-table">
                  <thead>
                    <tr><th>TIME</th><th>METRIC</th><th>VALUE</th></tr>
                  </thead>
                  <tbody>
                    {metrics.slice(0, 200).map(m => (
                      <tr key={m.id}>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)', whiteSpace: 'nowrap' }}>{format(new Date(m.timestamp), 'HH:mm:ss')}</td>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 10, color: 'var(--violet)' }}>{m.metric_name}</td>
                        <td style={{ fontFamily: 'var(--mono)', fontSize: 11, fontWeight: 700,
                          color: m.metric_name === 'cpu_percent' ? 'var(--cyan)' : m.metric_name === 'memory_mb' ? 'var(--lime)' : 'var(--amber)' }}>
                          {m.metric_value.toFixed(2)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}