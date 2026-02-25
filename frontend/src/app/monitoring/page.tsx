'use client';
import { useEffect, useState, useCallback, useRef } from 'react';
import {
  Activity, Cpu, HardDrive, Zap, Terminal, RefreshCcw, Clock,
  BarChart2, Filter, AlertTriangle,
} from 'lucide-react';
import { api, Log, Event, Metric, AlertCounts } from '@/lib/api';
import { useWebSocket } from '@/hooks/useWebSocket';
import { format, formatDistanceToNow } from 'date-fns';
import {
  AreaChart, Area, BarChart, Bar, XAxis, YAxis,
  CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts';
import clsx from 'clsx';

const RANGES = ['5m', '15m', '1h', '6h', '24h', '7d', '30d'] as const;
type Range = typeof RANGES[number];

interface ChartPt { label: string; cpu: number; mem: number; ev: number; }
interface LiveMetricEntry { time: string; agent: string; metric: string; value: number; }

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

export default function MonitoringPage() {
  const [range, setRange]           = useState<Range>('1h');
  const [logs, setLogs]             = useState<Log[]>([]);
  const [events, setEvents]         = useState<Event[]>([]);
  const [chart, setChart]           = useState<ChartPt[]>([]);
  const [liveMetrics, setLiveMetrics] = useState<LiveMetricEntry[]>([]);
  const [alertCounts, setAlertCounts] = useState<AlertCounts>({ active: 0, critical: 0, warnings: 0, resolved: 0 });
  const [logLevel, setLogLevel]     = useState<string>('ALL');
  const [tick, setTick]             = useState(0);
  const [pinLogs, setPinLogs]       = useState(true);
  const logEndRef   = useRef<HTMLDivElement>(null);
  const { connected, subscribe, throughput } = useWebSocket();
  const acc = useRef({ cpuSum: 0, cpuN: 0, memSum: 0, memN: 0, ev: 0 });

  const evPerSec  = (throughput['agents.events']  ?? 0).toFixed(1);
  const metPerSec = (throughput['metrics.stream'] ?? 0).toFixed(1);
  const logPerSec = (throughput['logs.stream']    ?? 0).toFixed(1);

  useEffect(() => {
    const t = setInterval(() => setTick(n => n + 1), 5000);
    return () => clearInterval(t);
  }, []);

  // Chart accumulation every 5s
  useEffect(() => {
    const a = acc.current;
    const avgCpu = a.cpuN ? a.cpuSum / a.cpuN : 0;
    const avgMem = a.memN ? a.memSum / a.memN : 0;
    setChart(prev => [...prev, { label: format(new Date(), 'HH:mm:ss'), cpu: avgCpu, mem: avgMem, ev: a.ev }].slice(-60));
    acc.current = { cpuSum: 0, cpuN: 0, memSum: 0, memN: 0, ev: 0 };
  }, [tick]);

  const fetchAll = useCallback(async () => {
    try {
      const [l, e, ac] = await Promise.all([
        api.logs.list(200),
        api.analytics.eventsHistory(range),
        api.alerts.counts(),
      ]);
      setLogs(l);
      setEvents(e as unknown as Event[]);
      setAlertCounts(ac);

      // Load historical metrics for the selected range
      try {
        const cpuHist = await api.metrics.history({ metric_name: 'cpu_percent', range, limit: 60 });
        const memHist = await api.metrics.history({ metric_name: 'memory_mb',   range, limit: 60 });
        if (cpuHist.length > 1) {
          // Merge CPU+mem into chart points by timestamp proximity
          const pts = cpuHist.slice().reverse().map((c, i) => ({
            label: format(new Date(c.timestamp), range === '7d' || range === '30d' ? 'MM/dd' : 'HH:mm'),
            cpu:   c.metric_value,
            mem:   memHist[memHist.length - 1 - i]?.metric_value ?? 0,
            ev:    0,
          }));
          setChart(pts);
        }
      } catch {}
    } catch {}
  }, [range]);

  useEffect(() => { fetchAll(); }, [fetchAll]);
  useEffect(() => {
    const t = setInterval(fetchAll, 20000);
    return () => clearInterval(t);
  }, [fetchAll]);

  // WS subscriptions
  useEffect(() => {
    const unsubLog = subscribe('logs.stream', msg => {
      const d = msg.data as { agent_id?: string; message?: string; level?: string; source?: string; timestamp?: string };
      if (!d.message) return;
      setLogs(prev => [{ id: crypto.randomUUID(), agent_id: d.agent_id || '', execution_id: null,
        level: d.level || 'info', message: d.message || '', source: d.source || null,
        metadata: {}, timestamp: d.timestamp || new Date().toISOString() }, ...prev].slice(0, 500));
    });
    const unsubMet = subscribe('metrics.stream', msg => {
      const d = msg.data as { agent_id?: string; metric_name?: string; metric_value?: number; timestamp?: string };
      if (d.metric_name === 'cpu_percent') { acc.current.cpuSum += d.metric_value || 0; acc.current.cpuN++; }
      if (d.metric_name === 'memory_mb')   { acc.current.memSum += d.metric_value || 0; acc.current.memN++; }
      setLiveMetrics(prev => [{
        time: format(new Date(), 'HH:mm:ss'), agent: (d.agent_id || '').slice(0,8),
        metric: d.metric_name || '', value: d.metric_value || 0,
      }, ...prev].slice(0, 100));
    });
    const unsubEv = subscribe('agents.events', msg => {
      const d = msg.data as { agent_id?: string; event_type?: string; source?: string; created_at?: string };
      acc.current.ev++;
      setEvents(prev => [{ id: crypto.randomUUID(), agent_id: d.agent_id || '',
        event_type: d.event_type || '', payload: {}, source: d.source || null,
        created_at: d.created_at || new Date().toISOString() }, ...prev].slice(0, 500));
    });
    return () => { unsubLog(); unsubMet(); unsubEv(); };
  }, [subscribe]);

  useEffect(() => {
    if (pinLogs) logEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [logs, pinLogs]);

  const filteredLogs = logLevel === 'ALL' ? logs : logs.filter(l => l.level.toLowerCase() === logLevel.toLowerCase());

  return (
    <div style={{ height: '100%', overflowY: 'auto', background: 'var(--base)' }}>
      {/* Header */}
      <div style={{ position: 'sticky', top: 0, zIndex: 40, background: 'rgba(3,7,15,0.97)', backdropFilter: 'blur(20px)', borderBottom: '1px solid var(--line-0)' }}>
        <div style={{ height: 1, background: 'linear-gradient(90deg,var(--cyan),var(--violet),var(--lime))', opacity: 0.4 }} />
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '7px 16px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <Activity style={{ width: 12, height: 12, color: 'var(--cyan)' }} />
            <span style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.1em' }}>OBSERVABILITY</span>

            {/* Throughput badges */}
            {[
              { label: 'EVENTS/s', val: evPerSec, color: 'var(--amber)' },
              { label: 'METRICS/s', val: metPerSec, color: 'var(--lime)' },
              { label: 'LOGS/s', val: logPerSec, color: 'var(--violet)' },
            ].map(({ label, val, color }) => (
              <div key={label} style={{ padding: '3px 8px', border: '1px solid var(--line-1)', background: 'var(--surface-0)', fontSize: 8.5, color, fontFamily: 'var(--mono)' }}>
                {label}: <span style={{ fontWeight: 700 }}>{val}</span>
              </div>
            ))}

            {/* Alert indicator */}
            {alertCounts.active > 0 && (
              <div style={{ padding: '3px 8px', background: 'rgba(255,61,85,0.08)', border: '1px solid rgba(255,61,85,0.3)', fontSize: 8.5, color: 'var(--crimson)', fontFamily: 'var(--mono)', fontWeight: 700, display: 'flex', alignItems: 'center', gap: 5 }}>
                <AlertTriangle style={{ width: 9, height: 9 }} />
                {alertCounts.active} ACTIVE ALERT{alertCounts.active > 1 ? 'S' : ''}
              </div>
            )}
          </div>

          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            {/* Time range selector */}
            <div style={{ display: 'flex', border: '1px solid var(--line-2)', overflow: 'hidden' }}>
              {RANGES.map(r => (
                <button key={r} onClick={() => setRange(r)} style={{
                  padding: '4px 8px', background: range === r ? 'rgba(0,217,255,0.12)' : 'var(--surface-0)',
                  border: 'none', borderRight: '1px solid var(--line-1)',
                  color: range === r ? 'var(--cyan)' : 'var(--ink-3)',
                  fontFamily: 'var(--mono)', fontSize: 9, fontWeight: range === r ? 700 : 400,
                  cursor: 'pointer', transition: 'all 0.15s',
                }}>{r}</button>
              ))}
            </div>
            <button onClick={fetchAll} className="op-btn op-btn-ghost" style={{ padding: '4px 10px', fontSize: 9 }}>
              <RefreshCcw style={{ width: 9, height: 9 }} /> REFRESH
            </button>
          </div>
        </div>
      </div>

      <div style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>
        {/* Charts row */}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 10 }}>
          {/* CPU chart */}
          <div className="panel">
            <div className="panel-header">
              <Cpu style={{ width: 10, height: 10, color: 'var(--cyan)' }} />
              <span>CPU UTILIZATION</span>
              <span style={{ marginLeft: 'auto', color: 'var(--cyan)', fontFamily: 'var(--mono)', fontSize: 12, fontWeight: 700 }}>
                {chart[chart.length-1]?.cpu.toFixed(1) ?? '0'}%
              </span>
            </div>
            <div className="chart-area" style={{ padding: '8px 4px 4px', minHeight: 120 }}>
              {chart.length < 2 ? (
                <div style={{ height: 110, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 9.5 }}>▶ AWAITING DATA</div>
              ) : (
                <ResponsiveContainer width="100%" height={110}>
                  <AreaChart data={chart} margin={{ top: 4, right: 4, bottom: 0, left: -20 }}>
                    <defs><linearGradient id="mc-cpu" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="var(--cyan)" stopOpacity={0.4}/><stop offset="95%" stopColor="var(--cyan)" stopOpacity={0}/></linearGradient></defs>
                    <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,.95)" vertical={false} />
                    <XAxis dataKey="label" stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} interval="preserveStartEnd" />
                    <YAxis stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} width={28} />
                    <Tooltip {...TT} formatter={(v: number) => [`${v.toFixed(1)}%`, 'CPU']} />
                    <Area type="monotone" dataKey="cpu" stroke="var(--cyan)" strokeWidth={1.5} fill="url(#mc-cpu)" dot={false} isAnimationActive={false} />
                  </AreaChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>

          {/* Memory chart */}
          <div className="panel">
            <div className="panel-header">
              <HardDrive style={{ width: 10, height: 10, color: 'var(--lime)' }} />
              <span>MEMORY USAGE</span>
              <span style={{ marginLeft: 'auto', color: 'var(--lime)', fontFamily: 'var(--mono)', fontSize: 12, fontWeight: 700 }}>
                {chart[chart.length-1]?.mem.toFixed(0) ?? '0'}MB
              </span>
            </div>
            <div className="chart-area" style={{ padding: '8px 4px 4px', minHeight: 120 }}>
              {chart.length < 2 ? (
                <div style={{ height: 110, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 9.5 }}>▶ AWAITING DATA</div>
              ) : (
                <ResponsiveContainer width="100%" height={110}>
                  <AreaChart data={chart} margin={{ top: 4, right: 4, bottom: 0, left: -20 }}>
                    <defs><linearGradient id="mc-mem" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="var(--lime)" stopOpacity={0.4}/><stop offset="95%" stopColor="var(--lime)" stopOpacity={0}/></linearGradient></defs>
                    <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,.95)" vertical={false} />
                    <XAxis dataKey="label" stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} interval="preserveStartEnd" />
                    <YAxis stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} width={28} />
                    <Tooltip {...TT} formatter={(v: number) => [`${v.toFixed(0)}MB`, 'Memory']} />
                    <Area type="monotone" dataKey="mem" stroke="var(--lime)" strokeWidth={1.5} fill="url(#mc-mem)" dot={false} isAnimationActive={false} />
                  </AreaChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>

          {/* Event throughput chart */}
          <div className="panel">
            <div className="panel-header">
              <Zap style={{ width: 10, height: 10, color: 'var(--amber)' }} />
              <span>EVENT THROUGHPUT</span>
              <span style={{ marginLeft: 'auto', color: 'var(--amber)', fontFamily: 'var(--mono)', fontSize: 12, fontWeight: 700 }}>
                {evPerSec}/s
              </span>
            </div>
            <div className="chart-area" style={{ padding: '8px 4px 4px', minHeight: 120 }}>
              {chart.length < 2 ? (
                <div style={{ height: 110, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 9.5 }}>▶ AWAITING DATA</div>
              ) : (
                <ResponsiveContainer width="100%" height={110}>
                  <BarChart data={chart} margin={{ top: 4, right: 4, bottom: 0, left: -20 }}>
                    <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,.95)" vertical={false} />
                    <XAxis dataKey="label" stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} interval="preserveStartEnd" />
                    <YAxis stroke="transparent" tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }} tickLine={false} width={28} />
                    <Tooltip {...TT} formatter={(v: number) => [v, 'Events']} />
                    <Bar dataKey="ev" fill="var(--amber)" opacity={0.7} isAnimationActive={false} />
                  </BarChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>
        </div>

        {/* Main data panels */}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
          {/* Log stream */}
          <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
            <div className="panel-header">
              <Terminal style={{ width: 10, height: 10, color: 'var(--violet)' }} />
              <span>LOG STREAM</span>
              <span style={{ fontSize: 9, color: 'var(--ink-3)', marginLeft: 4 }}>({filteredLogs.length})</span>
              <div style={{ marginLeft: 'auto', display: 'flex', gap: 3 }}>
                {(['ALL','ERROR','WARN','INFO','DEBUG'] as const).map(l => (
                  <button key={l} onClick={() => setLogLevel(l)} style={{
                    padding: '1px 5px', fontFamily: 'var(--mono)', fontSize: 7.5, fontWeight: 700,
                    background: logLevel === l ? 'rgba(0,217,255,0.12)' : 'transparent',
                    border: `1px solid ${logLevel === l ? 'rgba(0,217,255,0.3)' : 'var(--line-1)'}`,
                    color: logLevel === l ? 'var(--cyan)' : 'var(--ink-3)', cursor: 'pointer',
                  }}>{l}</button>
                ))}
              </div>
            </div>
            <div className="terminal-view" style={{ height: 320 }}>
              <div className="scanline" />
              <div
                onScroll={e => { const el = e.currentTarget; setPinLogs(el.scrollHeight - el.scrollTop - el.clientHeight < 40); }}
                style={{ height: '100%', overflowY: 'auto', padding: '4px 0', position: 'relative', zIndex: 3 }}
              >
                {filteredLogs.length === 0 ? (
                  <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--ink-3)', fontSize: 10 }}>
                    <span style={{ color: 'var(--lime)' }}>$</span>&nbsp;AWAITING LOG STREAM&nbsp;<span className="anim-cursor" style={{ color: 'var(--lime)' }}>█</span>
                  </div>
                ) : [...filteredLogs].reverse().map(log => {
                  const cfg = LOG_CFG[log.level] ?? LOG_CFG.debug;
                  return (
                    <div key={log.id} className={clsx('log-entry', log.level === 'error' && 'lv-error', (log.level === 'warn'||log.level === 'warning') && 'lv-warn')}>
                      <span className="log-ts">{format(new Date(log.timestamp), 'HH:mm:ss')}</span>
                      <span className="log-lvl" style={{ background: cfg.bg, border: `1px solid ${cfg.border}`, color: cfg.color }}>{log.level.slice(0,4).toUpperCase()}</span>
                      {log.source && <span className="log-src">[{log.source.slice(0,12)}]</span>}
                      <span className="log-msg">{log.message}</span>
                    </div>
                  );
                })}
                <div ref={logEndRef} />
              </div>
            </div>
          </div>

          {/* Right side: events + metrics */}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            {/* Event stream */}
            <div className="panel" style={{ display: 'flex', flexDirection: 'column', flex: 1 }}>
              <div className="panel-header">
                <Zap style={{ width: 10, height: 10, color: 'var(--amber)' }} />
                <span>EVENT STREAM</span>
                <span style={{ fontSize: 9, color: 'var(--ink-3)', marginLeft: 4 }}>({events.length})</span>
                <span style={{ marginLeft: 'auto', fontSize: 9, color: 'var(--amber)' }}>{evPerSec}/s</span>
              </div>
              <div style={{ overflowY: 'auto', maxHeight: 170 }}>
                {events.length === 0 ? (
                  <div style={{ padding: 24, textAlign: 'center', color: 'var(--ink-3)', fontSize: 10 }}>AWAITING EVENTS</div>
                ) : events.slice(0, 80).map(ev => (
                  <div key={ev.id} className="feed-item">
                    <span style={{ fontFamily: 'var(--mono)', fontSize: 8.5, color: 'var(--ink-3)', whiteSpace: 'nowrap' }}>
                      {format(new Date(ev.created_at), 'HH:mm:ss')}
                    </span>
                    <span style={{ fontFamily: 'var(--mono)', fontSize: 9, color: ev.event_type.includes('fail')||ev.event_type.includes('error') ? 'var(--crimson)' : ev.event_type.includes('start')||ev.event_type.includes('complete') ? 'var(--lime)' : 'var(--cyan)', fontWeight: 700 }}>
                      {ev.event_type.toUpperCase()}
                    </span>
                    <span style={{ fontSize: 8.5, color: 'var(--violet)', fontFamily: 'var(--mono)' }}>{ev.agent_id.slice(0,8)}</span>
                  </div>
                ))}
              </div>
            </div>

            {/* Live metrics table */}
            <div className="panel" style={{ display: 'flex', flexDirection: 'column', flex: 1 }}>
              <div className="panel-header">
                <BarChart2 style={{ width: 10, height: 10, color: 'var(--lime)' }} />
                <span>LIVE METRICS</span>
                <span style={{ fontSize: 9, color: 'var(--ink-3)', marginLeft: 4 }}>({liveMetrics.length})</span>
                <span style={{ marginLeft: 'auto', fontSize: 9, color: 'var(--lime)' }}>{metPerSec}/s</span>
              </div>
              <div style={{ overflowY: 'auto', maxHeight: 170 }}>
                {liveMetrics.length === 0 ? (
                  <div style={{ padding: 24, textAlign: 'center', color: 'var(--ink-3)', fontSize: 10 }}>
                    START AN AGENT TO BEGIN COLLECTION
                  </div>
                ) : (
                  <table className="op-table">
                    <thead>
                      <tr><th>TIME</th><th>AGENT</th><th>METRIC</th><th>VALUE</th></tr>
                    </thead>
                    <tbody>
                      {liveMetrics.slice(0, 100).map((m, i) => (
                        <tr key={i}>
                          <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)' }}>{m.time}</td>
                          <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--violet)' }}>{m.agent}</td>
                          <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: m.metric === 'cpu_percent' ? 'var(--cyan)' : m.metric === 'memory_mb' ? 'var(--lime)' : 'var(--amber)' }}>{m.metric}</td>
                          <td style={{ fontFamily: 'var(--mono)', fontSize: 11, fontWeight: 700, color: m.metric === 'cpu_percent' ? 'var(--cyan)' : m.metric === 'memory_mb' ? 'var(--lime)' : 'var(--amber)' }}>{m.value.toFixed(1)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}