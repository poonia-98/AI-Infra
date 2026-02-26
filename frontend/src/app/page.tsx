'use client';
import {
  useEffect, useState, useCallback, useRef,
} from 'react';
import Link from 'next/link';
import {
  Bot, Cpu, HardDrive, Zap, Activity, Database, Server, Radio,
  AlertTriangle, CheckCircle, Play, BarChart2, Terminal,
  Layers, Shield, RefreshCw, Clock, Network, Gauge,
  ArrowUpRight, Minus, TrendingUp,
} from 'lucide-react';
import {
  AreaChart, Area, LineChart, Line, BarChart, Bar,
  XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts';
import { api, SystemHealth, Agent, Log, Alert } from '@/lib/api';
import { useWebSocket } from '@/hooks/useWebSocket';
import { format, formatDistanceToNow } from 'date-fns';
import clsx from 'clsx';

/* ── Types ──────────────────────────────────────────────────── */
interface ChartPt  { t: number; label: string; cpu: number; mem: number; ev: number; ex: number; }
interface SparkPt  { i: number; v: number; }
interface AgentLive { cpu: number; mem: number; execs: number; seen: string; }
interface Feed {
  id: string; ts: number; type: string; agentId: string;
  label: string; level: 'ok' | 'info' | 'warn' | 'error'; color: string;
}

/* ── Event classification map ───────────────────────────────── */
const EV: Record<string, { label: string; level: Feed['level']; color: string; icon: string }> = {
  'agent.started':   { label: 'AGENT STARTED',   level: 'ok',    color: 'var(--lime)',    icon: '▶' },
  'agent.completed': { label: 'EXEC COMPLETE',    level: 'ok',    color: 'var(--teal)',    icon: '✓' },
  'agent.stopped':   { label: 'AGENT STOPPED',    level: 'info',  color: 'var(--cyan)',    icon: '■' },
  'agent.created':   { label: 'AGENT CREATED',    level: 'info',  color: 'var(--cyan)',    icon: '+' },
  'agent.deleted':   { label: 'AGENT DELETED',    level: 'warn',  color: 'var(--amber)',   icon: '✕' },
  'agent.failed':    { label: 'AGENT FAILED',     level: 'error', color: 'var(--crimson)', icon: '!' },
  'agent.heartbeat': { label: 'HEARTBEAT',        level: 'info',  color: 'var(--ink-3)',   icon: '♥' },
  'log.error':       { label: 'RUNTIME ERROR',    level: 'error', color: 'var(--crimson)', icon: '✕' },
  'execution.start': { label: 'EXEC STARTED',     level: 'ok',    color: 'var(--lime)',    icon: '►' },
  'execution.done':  { label: 'EXEC DONE',        level: 'ok',    color: 'var(--teal)',    icon: '✓' },
};

const LEVEL_DOT: Record<string, string> = {
  ok: 'beacon-online', info: 'beacon-active', warn: 'beacon-warn', error: 'beacon-error',
};

const LEVEL_BG: Record<string, string> = {
  ok:    'rgba(57,255,20,0.03)',
  info:  'rgba(0,217,255,0.03)',
  warn:  'rgba(255,184,0,0.04)',
  error: 'rgba(255,61,85,0.05)',
};

const LOG_CFG: Record<string, { color: string; bg: string; border: string }> = {
  error:   { color: 'var(--crimson)', bg: 'rgba(255,61,85,0.14)',  border: 'rgba(255,61,85,0.35)' },
  warn:    { color: 'var(--amber)',   bg: 'rgba(255,184,0,0.12)',  border: 'rgba(255,184,0,0.35)' },
  warning: { color: 'var(--amber)',   bg: 'rgba(255,184,0,0.12)',  border: 'rgba(255,184,0,0.35)' },
  info:    { color: 'var(--cyan)',    bg: 'rgba(0,217,255,0.10)',  border: 'rgba(0,217,255,0.3)' },
  debug:   { color: 'var(--ink-3)',   bg: 'transparent',           border: 'transparent' },
};

const STATUS_TAG: Record<string, string> = {
  running: 'tag-running', stopped: 'tag-stopped', created: 'tag-created',
  error: 'tag-error', failed: 'tag-failed', idle: 'tag-idle',
  pending: 'tag-pending', completed: 'tag-completed',
};

const TT = {
  contentStyle: {
    background: '#010306', border: '1px solid #102036',
    fontSize: 10, fontFamily: '"JetBrains Mono",monospace', color: '#7ba8c8',
    boxShadow: '0 8px 32px rgba(0,0,0,0.7)', padding: '6px 10px', borderRadius: 0,
  },
  labelStyle: { color: '#1e3d54', marginBottom: 3, letterSpacing: '0.1em' },
  cursor: { stroke: 'rgba(0,217,255,0.12)', strokeWidth: 1 },
};

/* ── Helpers ─────────────────────────────────────────────────── */
function classify(type: string) {
  return EV[type] ?? { label: type.toUpperCase(), level: 'info' as const, color: 'var(--cyan)', icon: '·' };
}
function fmtUp(s: number) {
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = Math.floor(s % 60);
  return `${h}h ${String(m).padStart(2,'0')}m ${String(sec).padStart(2,'0')}s`;
}
function svc(v?: string): 'online'|'warn'|'error'|'dead' {
  if (!v) return 'dead';
  if (v === 'healthy') return 'online';
  if (v === 'unavailable') return 'error';
  return 'warn';
}

/* ── Sub-components ──────────────────────────────────────────── */

function Beacon({ state }: { state: 'online'|'active'|'warn'|'error'|'dead' }) {
  return (
    <div className="beacon" style={{ width: 14, height: 14, flexShrink: 0 }}>
      <div className={`beacon-core beacon-${state}`} style={{ width: 7, height: 7 }} />
      {(state === 'online' || state === 'active') && (
        <div className={`beacon-ring beacon-${state}`}
          style={{ position: 'absolute', inset: 0, borderRadius: '50%' }} />
      )}
    </div>
  );
}

function Spark({ data, color, height = 32 }: { data: SparkPt[]; color: string; height?: number }) {
  if (data.length < 2) return (
    <div style={{ height, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div style={{ width: '100%', height: 1, background: 'var(--line-1)' }} />
    </div>
  );
  return (
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={data} margin={{ top: 2, right: 2, bottom: 2, left: 2 }}>
        <Line type="monotone" dataKey="v" stroke={color} strokeWidth={1.5}
          dot={false} isAnimationActive={false} />
      </LineChart>
    </ResponsiveContainer>
  );
}

function KpiCard({
  label, value, sub, icon: Icon, color, sparks, accent,
}: {
  label: string; value: string|number; sub?: string;
  icon: React.ComponentType<{ style?: React.CSSProperties }>;
  color: string; sparks: SparkPt[]; accent: string;
}) {
  const val = String(value);
  const active = val !== '0' && val !== '0.0' && val !== '—';
  const prevRef = useRef(val);
  const [flashing, setFlashing] = useState(false);
  useEffect(() => {
    if (prevRef.current !== val) {
      setFlashing(true);
      const t = setTimeout(() => setFlashing(false), 500);
      prevRef.current = val;
      return () => clearTimeout(t);
    }
  }, [val]);

  return (
    <div className={clsx('kpi-card', `kpi-${color}`, active && 'kpi-active')}>
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 4 }}>
        <div style={{ fontSize: '8px', letterSpacing: '0.14em', fontWeight: 700, color: 'var(--ink-3)', lineHeight: 1.3 }}>
          {label}
        </div>
        <Icon style={{ width: 11, height: 11, color: active ? accent : 'var(--ink-3)', flexShrink: 0, marginTop: 1 }} />
      </div>
      <div style={{
        fontFamily: 'var(--mono)', fontSize: '1.45rem', fontWeight: 700,
        lineHeight: 1, marginBottom: 2,
        color: active ? accent : 'var(--ink-2)',
        textShadow: active ? `0 0 20px ${accent}55` : undefined,
        fontVariantNumeric: 'tabular-nums',
        animation: flashing ? 'value-flash 0.5s ease-out' : undefined,
      }}>
        {value}
      </div>
      {sub && (
        <div style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.06em', marginBottom: 2 }}>{sub}</div>
      )}
      <div style={{ marginTop: 4, opacity: sparks.length > 1 ? 1 : 0.3 }}>
        <Spark data={sparks} color={active ? accent : 'var(--ink-3)'} height={26} />
      </div>

      {/* ════════ FLOATING DEPLOY BUTTON ════════ */}
      <button className="deploy-fab" onClick={() => window.location.href = '/agents'}>
        <span style={{ fontSize: 14, lineHeight: 1 }}>+</span>
        DEPLOY AGENT
      </button>

    </div>
  );
}

function SvcRow({ label, status, icon: Icon }: {
  label: string; status: 'online'|'warn'|'error'|'dead';
  icon: React.ComponentType<{ style?: React.CSSProperties }>;
}) {
  const cfg = {
    online: { text: 'HEALTHY',  color: 'var(--lime)',    bg: 'rgba(57,255,20,0.07)',  bd: 'rgba(57,255,20,0.2)' },
    warn:   { text: 'DEGRADED', color: 'var(--amber)',   bg: 'rgba(255,184,0,0.07)', bd: 'rgba(255,184,0,0.2)' },
    error:  { text: 'FAULT',    color: 'var(--crimson)', bg: 'rgba(255,61,85,0.07)', bd: 'rgba(255,61,85,0.2)' },
    dead:   { text: 'UNKNOWN',  color: 'var(--ink-3)',   bg: 'rgba(13,30,51,0.4)',   bd: 'var(--line-1)' },
  }[status];
  return (
    <div className="svc-node">
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <Beacon state={status} />
        <Icon style={{ width: 11, height: 11, color: 'var(--ink-3)' }} />
        <span style={{ fontSize: 11, color: 'var(--ink-1)', letterSpacing: '0.03em' }}>{label}</span>
      </div>
      <span style={{
        fontSize: '8px', fontWeight: 700, letterSpacing: '0.12em',
        padding: '2px 6px', background: cfg.bg, border: `1px solid ${cfg.bd}`, color: cfg.color,
      }}>
        {cfg.text}
      </span>
    </div>
  );
}

function GaugeRow({ value, color }: { value: number; color: string }) {
  const pct = Math.min(100, Math.max(0, value));
  const gc = pct > 80 ? 'var(--crimson)' : pct > 60 ? 'var(--amber)' : color;
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
      <div className="gauge-track" style={{ flex: 1 }}>
        <div className="gauge-fill" style={{ width: `${pct}%`, background: gc }} />
      </div>
      <span style={{ fontSize: 9, color: gc, width: 30, textAlign: 'right', fontFamily: 'var(--mono)', fontWeight: 600 }}>
        {value.toFixed(0)}%
      </span>
    </div>
  );
}

function AreaPanel({ title, icon, color, dataKey, data, unit, id: gId }: {
  title: string; icon: React.ReactNode; color: string;
  dataKey: 'cpu'|'mem'|'ev'|'ex'; data: ChartPt[]; unit: string; id: string;
}) {
  const latest = data.length ? data[data.length - 1][dataKey] : 0;
  const hasData = data.length >= 2;
  return (
    <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
      <div className="panel-header">
        {icon}
        <span style={{ letterSpacing: '0.12em' }}>{title}</span>
        {hasData && (
          <span style={{ marginLeft: 'auto', color, fontSize: 13, fontWeight: 700, fontFamily: 'var(--mono)' }}>
            {typeof latest === 'number' ? latest.toFixed(1) : latest}
            <span style={{ fontSize: 9, fontWeight: 400, color: 'var(--ink-3)', marginLeft: 3 }}>{unit}</span>
          </span>
        )}
      </div>
      <div className="chart-area" style={{ flex: 1, padding: '8px 4px 4px', minHeight: 140 }}>
        {!hasData ? (
          <div style={{ height: 130, display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 8 }}>
            <div style={{ color: 'var(--ink-3)', fontSize: 9.5, letterSpacing: '0.12em', fontFamily: 'var(--mono)' }}>
              ▶ AWAITING DATA STREAM
            </div>
            <div style={{ display: 'flex', gap: 3 }}>
              {[0,1,2,3,4].map(i => (
                <div key={i} style={{
                  width: 3, height: 14,
                  background: color,
                  opacity: 0.3,
                  animation: `pulse-beacon ${0.8 + i * 0.15}s ease-in-out infinite`,
                  animationDelay: `${i * 0.12}s`,
                }} />
              ))}
            </div>
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={130}>
            <AreaChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: -22 }}>
              <defs>
                <linearGradient id={gId} x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%"  stopColor={color} stopOpacity={0.4} />
                  <stop offset="95%" stopColor={color} stopOpacity={0.01} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,0.95)" vertical={false} />
              <XAxis dataKey="label" stroke="transparent"
                tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }}
                tickLine={false} interval="preserveStartEnd" />
              <YAxis stroke="transparent"
                tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }}
                tickLine={false} width={28} />
              <Tooltip {...TT} formatter={(v: number) => [`${v.toFixed(1)}${unit}`, title]} />
              <Area type="monotone" dataKey={dataKey} stroke={color} strokeWidth={1.5}
                fill={`url(#${gId})`} dot={false} isAnimationActive={false}
                activeDot={{ r: 3, fill: color, strokeWidth: 0 }} />
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>

      {/* ════════ FLOATING DEPLOY BUTTON ════════ */}
      <button className="deploy-fab" onClick={() => window.location.href = '/agents'}>
        <span style={{ fontSize: 14, lineHeight: 1 }}>+</span>
        DEPLOY AGENT
      </button>

    </div>
  );
}

function BarPanel({ title, icon, color, dataKey, data }: {
  title: string; icon: React.ReactNode; color: string;
  dataKey: 'ev'|'ex'; data: ChartPt[];
}) {
  const latest = data.length ? data[data.length - 1][dataKey] : 0;
  const hasData = data.length >= 2;
  return (
    <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
      <div className="panel-header">
        {icon}
        <span style={{ letterSpacing: '0.12em' }}>{title}</span>
        {hasData && (
          <span style={{ marginLeft: 'auto', color, fontSize: 13, fontWeight: 700, fontFamily: 'var(--mono)' }}>
            {latest}
            <span style={{ fontSize: 9, fontWeight: 400, color: 'var(--ink-3)', marginLeft: 3 }}>/5s</span>
          </span>
        )}
      </div>
      <div className="chart-area" style={{ flex: 1, padding: '8px 4px 4px', minHeight: 140 }}>
        {!hasData ? (
          <div style={{ height: 130, display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 8 }}>
            <div style={{ color: 'var(--ink-3)', fontSize: 9.5, letterSpacing: '0.12em' }}>
              ▶ AWAITING DATA STREAM
            </div>
            <div style={{ display: 'flex', gap: 3 }}>
              {[0,1,2,3,4,5,6].map(i => (
                <div key={i} style={{
                  width: 6, height: Math.max(6, Math.sin(i * 0.9) * 24 + 28),
                  background: color, opacity: 0.15, borderRadius: 1,
                }} />
              ))}
            </div>
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={130}>
            <BarChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: -22 }} barSize={5}>
              <CartesianGrid strokeDasharray="2 10" stroke="rgba(10,24,40,0.95)" vertical={false} />
              <XAxis dataKey="label" stroke="transparent"
                tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }}
                tickLine={false} interval="preserveStartEnd" />
              <YAxis stroke="transparent"
                tick={{ fill: 'var(--ink-3)', fontSize: 8, fontFamily: '"JetBrains Mono",monospace' }}
                tickLine={false} width={28} />
              <Tooltip {...TT} formatter={(v: number) => [`${v}`, title]} />
              <Bar dataKey={dataKey} fill={color} fillOpacity={0.8}
                radius={[2,2,0,0]} isAnimationActive={false} />
            </BarChart>
          </ResponsiveContainer>
        )}
      </div>

      {/* ════════ FLOATING DEPLOY BUTTON ════════ */}
      <button className="deploy-fab" onClick={() => window.location.href = '/agents'}>
        <span style={{ fontSize: 14, lineHeight: 1 }}>+</span>
        DEPLOY AGENT
      </button>

    </div>
  );
}

function AgentRow({ agent, live, idx }: { agent: Agent; live?: AgentLive; idx: number }) {
  const isRunning = agent.status === 'running';
  return (
    <tr>
      <td style={{ color: 'var(--ink-3)', fontFamily: 'var(--mono)', fontSize: 9 }}>
        {String(idx + 1).padStart(2, '0')}
      </td>
      <td>
        <Link href={`/agents/${agent.id}`}
          style={{ color: 'var(--cyan)', fontSize: 10, fontFamily: 'var(--mono)', letterSpacing: '0.04em' }}>
          {agent.id.slice(0, 8)}
        </Link>
      </td>
      <td>
        <Link href={`/agents/${agent.id}`} style={{ color: 'var(--ink-0)', fontWeight: 500 }}>
          {agent.name}
        </Link>
      </td>
      <td>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          {isRunning && (
            <div className="beacon" style={{ width: 10, height: 10 }}>
              <div className="beacon-core beacon-online" style={{ width: 5, height: 5 }} />
            </div>
          )}
          <span className={`tag ${STATUS_TAG[agent.status] ?? 'tag-stopped'}`}>
            {agent.status}
          </span>
        </div>
      </td>
      <td>
        {live ? (
          <div style={{ width: 90 }}>
            <GaugeRow value={live.cpu} color="var(--cyan)" />
          </div>
        ) : <span style={{ color: 'var(--ink-3)' }}>—</span>}
      </td>
      <td>
        {live && live.mem > 0 ? (
          <div style={{ width: 90 }}>
            <GaugeRow value={Math.min(100, (live.mem / 512) * 100)} color="var(--lime)" />
          </div>
        ) : <span style={{ color: 'var(--ink-3)' }}>—</span>}
      </td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 10, color: 'var(--ink-2)' }}>
        {live?.execs ?? 0}
      </td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-3)' }}>
        {agent.container_id ? agent.container_id.slice(0, 12) : '—'}
      </td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-3)' }}>
        {live?.seen
          ? formatDistanceToNow(new Date(live.seen), { addSuffix: true })
          : formatDistanceToNow(new Date(agent.updated_at), { addSuffix: true })}
      </td>
      <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)' }}>
        {agent.agent_type}
      </td>
    </tr>
  );
}

function LogStream({ logs }: { logs: Log[] }) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const [pinned, setPinned] = useState(true);

  useEffect(() => {
    if (pinned) bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [logs, pinned]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 40);
  };

  return (
    <div className="terminal-view" style={{ height: 280 }}>
      <div className="scanline" />
      <div ref={scrollRef} style={{ height: '100%', overflowY: 'auto', padding: '4px 0', position: 'relative', zIndex: 3 }} onScroll={onScroll}>
        {logs.length === 0 ? (
          <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center', flexDirection: 'column', gap: 8 }}>
            <div style={{ color: 'var(--ink-3)', fontSize: 11, letterSpacing: '0.1em' }}>
              <span style={{ color: 'var(--lime)' }}>$</span>&nbsp;
              AWAITING LOG STREAM&nbsp;
              <span className="anim-cursor" style={{ color: 'var(--lime)' }}>█</span>
            </div>
            <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.08em' }}>
              START AN AGENT TO BEGIN RECEIVING LOGS
            </div>
          </div>
        ) : [...logs].reverse().map(log => {
          const cfg = LOG_CFG[log.level] ?? LOG_CFG.debug;
          const isErr = log.level === 'error';
          const isWarn = log.level === 'warn' || log.level === 'warning';
          return (
            <div key={log.id} className={clsx('log-entry', isErr && 'lv-error', isWarn && 'lv-warn')}>
              <span className="log-ts">{format(new Date(log.timestamp), 'HH:mm:ss.SSS').slice(0, 12)}</span>
              <span className="log-lvl"
                style={{ background: cfg.bg, border: `1px solid ${cfg.border}`, color: cfg.color }}>
                {log.level.slice(0, 4).toUpperCase()}
              </span>
              {log.source && <span className="log-src">[{log.source}]</span>}
              <span className="log-msg">{log.message}</span>
            </div>
          );
        })}
        <div ref={bottomRef} />
      </div>

      {/* ════════ FLOATING DEPLOY BUTTON ════════ */}
      <button className="deploy-fab" onClick={() => window.location.href = '/agents'}>
        <span style={{ fontSize: 14, lineHeight: 1 }}>+</span>
        DEPLOY AGENT
      </button>

    </div>
  );
}

/* ══════════════════════════════════════════════════════════════
   MAIN DASHBOARD
   ══════════════════════════════════════════════════════════════ */

export default function DashboardPage() {
  const [health, setHealth]       = useState<SystemHealth | null>(null);
  const [agents, setAgents]       = useState<Agent[]>([]);
  const [activeAlerts, setActiveAlerts] = useState<Alert[]>([]);
  const [feed, setFeed]           = useState<Feed[]>([]);
  const [logs, setLogs]           = useState<Log[]>([]);
  const [chart, setChart]         = useState<ChartPt[]>([]);
  const [agentLive, setAgentLive] = useState<Map<string, AgentLive>>(new Map());
  const [loading, setLoading]     = useState(true);
  const [tick, setTick]           = useState(0);

  const spk = useRef<Record<string, SparkPt[]>>({ cpu:[], mem:[], ev:[], ex:[], evRate:[], metRate:[] });
  const [sparks, setSparks] = useState(spk.current);
  const acc = useRef({ cpuSum: 0, cpuN: 0, memSum: 0, memN: 0, ev: 0, ex: 0 });

  const { connected, subscribe, throughput } = useWebSocket();

  /* 1-second tick */
  useEffect(() => {
    const id = setInterval(() => setTick(n => n + 1), 1000);
    return () => clearInterval(id);
  }, []);

  /* Sparklines every 2s */
  useEffect(() => {
    if (tick % 2 !== 0) return;
    const a = acc.current;
    const now = Date.now();
    const avgCpu = a.cpuN ? a.cpuSum / a.cpuN : spk.current.cpu.slice(-1)[0]?.v ?? 0;
    const avgMem = a.memN ? a.memSum / a.memN : spk.current.mem.slice(-1)[0]?.v ?? 0;
    const evR  = throughput['agents.events']  ?? 0;
    const mR   = throughput['metrics.stream'] ?? 0;
    const push = (arr: SparkPt[], v: number) => [...arr, { i: now, v }].slice(-20);
    spk.current = {
      cpu: push(spk.current.cpu, avgCpu), mem: push(spk.current.mem, avgMem),
      ev:  push(spk.current.ev, a.ev),   ex:  push(spk.current.ex, a.ex),
      evRate: push(spk.current.evRate, evR), metRate: push(spk.current.metRate, mR),
    };
    setSparks({ ...spk.current });
  }, [tick, throughput]);

  /* Chart every 5s */
  useEffect(() => {
    if (tick % 5 !== 0 || tick === 0) return;
    const a = acc.current;
    const pt: ChartPt = {
      t: Date.now(), label: format(new Date(), 'HH:mm:ss'),
      cpu: +(a.cpuN ? a.cpuSum / a.cpuN : 0).toFixed(1),
      mem: +(a.memN ? a.memSum / a.memN : 0).toFixed(1),
      ev: a.ev, ex: a.ex,
    };
    setChart(prev => [...prev, pt].slice(-60));
    acc.current = { cpuSum: 0, cpuN: 0, memSum: 0, memN: 0, ev: 0, ex: 0 };
  }, [tick]);

  /* Initial fetch */
  const fetchAll = useCallback(async () => {
    try {
      const [h, ags, evs, ls, als] = await Promise.all([
        api.system.health(), api.agents.list(),
        api.events.list(60), api.logs.list(100),
        api.alerts.list(false),
      ]);
      setHealth(h);
      setAgents(ags);
      setLogs(ls);
      setActiveAlerts(als);
      const items: Feed[] = evs
        .map(e => {
          const c = classify(e.event_type);
          return { id: e.id, ts: new Date(e.created_at).getTime(), type: e.event_type, agentId: e.agent_id, ...c };
        })
        .sort((a, b) => b.ts - a.ts).slice(0, 80);
      setFeed(items);
    } catch { /* keep existing */ }
    finally { setLoading(false); }
  }, []);

  useEffect(() => {
    fetchAll();
    const id = setInterval(() => {
      api.system.health().then(h => setHealth(h)).catch(() => {});
      api.agents.list().then(a => setAgents(a)).catch(() => {});
    }, 12000);
    return () => clearInterval(id);
  }, [fetchAll]);

  /* WebSocket subscriptions */
  useEffect(() => {
    const unsubEv = subscribe('agents.events', msg => {
      const d = msg.data;
      const type    = String(d.event_type ?? '');
      const agentId = String(d.agent_id   ?? '');
      if (!type || !agentId) return;
      if (type === 'agent.heartbeat') return; // don't pollute feed with heartbeats
      acc.current.ev++;
      const c = classify(type);
      setFeed(prev => [{ id: crypto.randomUUID(), ts: Date.now(), type, agentId, ...c }, ...prev].slice(0, 80));
      if (['agent.started','agent.stopped','agent.failed','agent.completed'].includes(type)) {
        api.agents.list().then(a => setAgents(a)).catch(() => {});
        api.system.health().then(h => setHealth(h)).catch(() => {});
        if (type === 'agent.completed' || type === 'agent.started') {
          setAgentLive(prev => {
            const next = new Map(prev);
            const cur = next.get(agentId);
            if (cur) next.set(agentId, { ...cur, execs: cur.execs + 1 });
            return next;
          });
          acc.current.ex++;
        }
      }
    });

    const unsubMet = subscribe('metrics.stream', msg => {
      const d = msg.data;
      // Collector publishes snapshot: {agent_id, cpu_percent, memory_mb, memory_percent, ...}
      // Also handle legacy single-metric format: {agent_id, metric_name, metric_value}
      const agentId = String(d.agent_id ?? '');
      if (!agentId) return;

      // Snapshot format (from metrics-collector)
      if (d.cpu_percent !== undefined || d.memory_mb !== undefined) {
        const cpu = Number(d.cpu_percent ?? 0);
        const mem = Number(d.memory_mb   ?? 0);
        acc.current.cpuSum += cpu; acc.current.cpuN++;
        acc.current.memSum += mem; acc.current.memN++;
        setAgentLive(prev => {
          const next = new Map(prev);
          const cur = next.get(agentId) ?? { cpu: 0, mem: 0, execs: 0, seen: '' };
          next.set(agentId, { ...cur, cpu, mem, seen: new Date().toISOString() });
          return next;
        });
        return;
      }

      // Legacy single-metric format
      const name = String(d.metric_name  ?? '');
      const val  = Number(d.metric_value ?? 0);
      if (name === 'cpu_percent') { acc.current.cpuSum += val; acc.current.cpuN++; }
      if (name === 'memory_mb')   { acc.current.memSum += val; acc.current.memN++; }
      setAgentLive(prev => {
        const next = new Map(prev);
        const cur = next.get(agentId) ?? { cpu: 0, mem: 0, execs: 0, seen: '' };
        next.set(agentId, {
          ...cur,
          cpu: name === 'cpu_percent' ? val : cur.cpu,
          mem: name === 'memory_mb'   ? val : cur.mem,
          seen: new Date().toISOString(),
        });
        return next;
      });
    });

    const unsubLog = subscribe('logs.stream', msg => {
      const d = msg.data;
      const log: Log = {
        id: crypto.randomUUID(),
        agent_id: String(d.agent_id ?? ''),
        execution_id: null,
        level:    String(d.level   ?? 'info'),
        message:  String(d.message ?? ''),
        source:   d.source ? String(d.source) : null,
        metadata: {},
        timestamp: String(d.timestamp ?? new Date().toISOString()),
      };
      setLogs(prev => [log, ...prev].slice(0, 200));
      if (log.level === 'error') {
        setFeed(prev => [{ id: crypto.randomUUID(), ts: Date.now(), type: 'log.error',
          agentId: log.agent_id, level: 'error', label: 'RUNTIME ERROR', color: 'var(--crimson)', icon: '!',
        }, ...prev].slice(0, 80) as Feed[]);
      }
    });

    return () => { unsubEv(); unsubMet(); unsubLog(); };
  }, [subscribe]);

  /* Derived */
  const totalAgents   = health?.total_agents     ?? 0;
  const runningAgents = health?.running_agents   ?? 0;
  const failedAgents  = health?.failed_agents    ?? 0;
  const stoppedAgents = Math.max(0, totalAgents - runningAgents - failedAgents);
  const totalExecs    = health?.total_executions ?? 0;
  const activeExecs   = health?.active_executions ?? 0;

  const liveVals  = Array.from(agentLive.values());
  const cpuVals   = liveVals.map(v => v.cpu).filter(v => v > 0);
  const memVals   = liveVals.map(v => v.mem).filter(v => v > 0);
  const avgCpu    = cpuVals.length ? cpuVals.reduce((a,b) => a+b, 0) / cpuVals.length : 0;
  const avgMem    = memVals.length ? memVals.reduce((a,b) => a+b, 0) / memVals.length : 0;
  const evPerSec  = (throughput['agents.events']  ?? 0).toFixed(1);
  const metPerSec = (throughput['metrics.stream'] ?? 0).toFixed(1);
  const cpPlane   = health?.status === 'healthy' ? 'online' : health ? 'warn' : 'dead';
  const now       = Date.now() + (tick * 0); // force re-read on tick

  return (
    <div style={{ height: '100%', overflowY: 'auto', background: 'var(--base)' }}>

      {/* ════════════════ STICKY HEADER ════════════════ */}
      <div style={{
        position: 'sticky', top: 0, zIndex: 40,
        background: 'rgba(3,7,15,0.97)', backdropFilter: 'blur(20px)',
        borderBottom: '1px solid var(--line-0)',
      }}>
        {/* Top accent line */}
        <div style={{ height: 1, background: 'linear-gradient(90deg, var(--cyan) 0%, var(--lime) 50%, var(--violet) 100%)', opacity: 0.5 }} />
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '6px 16px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 16 }}>
            <div>
              <div style={{ fontFamily: 'var(--display)', fontSize: 16, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.10em', lineHeight: 1 }}>
                MISSION CONTROL
              </div>
              <div style={{ fontSize: 9, color: 'var(--ink-3)', letterSpacing: '0.14em', fontFamily: 'var(--mono)', marginTop: 2 }}>
                {format(new Date(now), 'yyyy-MM-dd HH:mm:ss')} UTC
              </div>
            </div>

            <div style={{ width: 1, height: 30, background: 'var(--line-1)' }} />

            {/* Connection status */}
            <div style={{
              display: 'flex', alignItems: 'center', gap: 7,
              padding: '4px 10px',
              background: connected ? 'rgba(57,255,20,0.05)' : 'rgba(255,61,85,0.07)',
              border: `1px solid ${connected ? 'rgba(57,255,20,0.18)' : 'rgba(255,61,85,0.2)'}`,
            }}>
              <Beacon state={connected ? 'online' : 'error'} />
              <span style={{ fontSize: 9.5, fontWeight: 700, letterSpacing: '0.12em', color: connected ? 'var(--lime)' : 'var(--crimson)' }}>
                {connected ? 'STREAM LIVE' : 'STREAM OFFLINE'}
              </span>
              {connected && (
                <span style={{ fontSize: 9, color: 'var(--ink-3)', marginLeft: 2 }}>
                  {(parseFloat(evPerSec) + parseFloat(metPerSec)).toFixed(1)} msg/s
                </span>
              )}
            </div>

            {/* CP status */}
            <div style={{
              display: 'flex', alignItems: 'center', gap: 6,
              padding: '4px 10px',
              background: cpPlane === 'online' ? 'rgba(57,255,20,0.05)' : 'rgba(255,184,0,0.05)',
              border: `1px solid ${cpPlane === 'online' ? 'rgba(57,255,20,0.15)' : 'rgba(255,184,0,0.15)'}`,
            }}>
              <Shield style={{ width: 10, height: 10, color: cpPlane === 'online' ? 'var(--lime)' : 'var(--amber)' }} />
              <span style={{ fontSize: 9, fontWeight: 700, letterSpacing: '0.1em', color: cpPlane === 'online' ? 'var(--lime)' : 'var(--amber)' }}>
                {cpPlane === 'online' ? 'CONTROL PLANE HEALTHY' : 'CONTROL PLANE DEGRADED'}
              </span>
            </div>
          </div>

          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <div style={{
              display: 'flex', alignItems: 'center', gap: 6,
              padding: '4px 10px', background: 'var(--surface-0)', border: '1px solid var(--line-1)',
              fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-2)', letterSpacing: '0.08em',
            }}>
              <Clock style={{ width: 10, height: 10 }} />
              UP {health ? fmtUp(health.uptime_seconds) : '—'}
            </div>
            <button onClick={fetchAll} style={{
              display: 'flex', alignItems: 'center', gap: 5,
              padding: '4px 10px', background: 'var(--surface-0)', border: '1px solid var(--line-1)',
              fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-2)', letterSpacing: '0.1em',
              cursor: 'pointer',
            }}>
              <RefreshCw style={{ width: 10, height: 10 }} />
              SYNC
            </button>
          </div>
        </div>
      </div>

      <div style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>

        {/* ════════════════ KPI STRIP ════════════════ */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', gap: 6 }}>
          {[
            { label: 'TOTAL AGENTS', value: totalAgents,             icon: Bot,           color: 'cyan',    accent: 'var(--cyan)',    sp: spk.current.cpu.map((_,i) => ({i,v:totalAgents})) },
            { label: 'RUNNING',      value: runningAgents,           icon: Play,          color: 'lime',    accent: 'var(--lime)',    sp: spk.current.evRate },
            { label: 'STOPPED',      value: stoppedAgents,           icon: Minus,         color: 'cyan',    accent: 'var(--ink-2)',   sp: [] },
            { label: 'FAILED',       value: failedAgents,            icon: AlertTriangle, color: 'crimson', accent: 'var(--crimson)', sp: [] },
            { label: 'CONTAINERS',   value: runningAgents,           icon: Layers,        color: 'teal',    accent: 'var(--teal)',    sp: spk.current.evRate, sub: 'active' },
            { label: 'AVG CPU',      value: `${avgCpu.toFixed(1)}%`, icon: Cpu,           color: avgCpu > 80 ? 'crimson' : 'cyan', accent: 'var(--cyan)', sp: sparks.cpu },
            { label: 'AVG MEM',      value: `${avgMem.toFixed(0)}MB`, icon: HardDrive,   color: avgMem > 400 ? 'crimson' : 'lime', accent: 'var(--lime)', sp: sparks.mem },
            { label: 'EVENTS/s',     value: evPerSec,                icon: Zap,           color: 'amber',   accent: 'var(--amber)',   sp: sparks.evRate },
            { label: 'METRICS/s',    value: metPerSec,               icon: BarChart2,     color: 'violet',  accent: 'var(--violet)',  sp: sparks.metRate },
          ].map(({ label, value, icon: Icon, color, accent, sp, sub }) => (
            <KpiCard key={label} label={label} value={value} sub={sub}
              icon={Icon as React.ComponentType<{ style?: React.CSSProperties }>}
              color={color} accent={accent} sparks={sp ?? []} />
          ))}
        </div>

        {/* ════════════════ ACTIVITY FEED + SERVICE HEALTH ════════════════ */}
        <div className="grid-2col" style={{ display: 'grid', gridTemplateColumns: '3fr 2fr', gap: 10 }}>

          {/* Activity feed */}
          <div className="panel panel-glow" style={{ height: 340, display: 'flex', flexDirection: 'column' }}>
            <div className="panel-header">
              <Beacon state="active" />
              <span style={{ letterSpacing: '0.14em' }}>LIVE ACTIVITY STREAM</span>
              <span style={{
                marginLeft: 'auto', fontSize: '8px', padding: '2px 7px',
                background: 'var(--surface-0)', border: '1px solid var(--line-1)', color: 'var(--ink-3)',
              }}>
                {feed.length} EVENTS
              </span>
            </div>
            <div style={{ flex: 1, overflowY: 'auto' }}>
              {feed.length === 0 ? (
                <div style={{ height: '100%', display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 8 }}>
                  <div style={{ color: 'var(--ink-3)', fontSize: 10, letterSpacing: '0.1em' }}>▶ AWAITING EVENTS</div>
                  <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.08em' }}>
                    CREATE AND START AN AGENT TO SEE LIVE EVENTS
                  </div>
                </div>
              ) : feed.map((item, i) => (
                <div key={item.id}
                  className={clsx('feed-item', i === 0 && 'feed-new')}
                  style={{ background: i === 0 ? LEVEL_BG[item.level] : undefined }}>
                  <span style={{ fontSize: 9, color: 'var(--ink-3)', fontFamily: 'var(--mono)', flexShrink: 0, width: 56 }}>
                    {format(new Date(item.ts), 'HH:mm:ss')}
                  </span>
                  <div className="beacon" style={{ width: 10, height: 10 }}>
                    <div className={`beacon-core ${LEVEL_DOT[item.level]}`} style={{ width: 5, height: 5 }} />
                  </div>
                  <span style={{
                    fontFamily: 'var(--mono)', fontSize: 9, letterSpacing: '0.06em',
                    color: item.color, width: 24, flexShrink: 0, textAlign: 'center',
                  }}>{(item as any).icon ?? '·'}</span>
                  <span style={{ fontWeight: 700, fontSize: 9.5, letterSpacing: '0.08em', color: item.color, width: 128, flexShrink: 0 }}>
                    {item.label}
                  </span>
                  <span style={{
                    fontSize: 9, padding: '1px 5px',
                    background: 'rgba(0,217,255,0.05)',
                    border: '1px solid rgba(0,217,255,0.1)', color: 'var(--cyan)', fontFamily: 'var(--mono)',
                  }}>
                    {item.agentId.slice(0, 8)}
                  </span>
                  <span style={{ fontSize: 9, color: 'var(--ink-4)', marginLeft: 'auto', flexShrink: 0, fontFamily: 'var(--mono)' }}>
                    {formatDistanceToNow(item.ts, { addSuffix: true })}
                  </span>
                </div>
              ))}
            </div>
          </div>

          {/* Right col */}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8, height: 340 }}>

            {/* System topology */}
            <div className="panel" style={{ flex: 1, overflow: 'hidden' }}>
              <div className="panel-header">
                <Shield style={{ width: 10, height: 10, color: 'var(--cyan)' }} />
                <span style={{ letterSpacing: '0.14em' }}>SYSTEM TOPOLOGY</span>
              </div>
              <SvcRow label="Control Plane"  status={cpPlane}                    icon={CheckCircle} />
              <SvcRow label="PostgreSQL DB"  status={svc(health?.services?.database)} icon={Database} />
              <SvcRow label="Redis Cache"    status={svc(health?.services?.redis)}    icon={Server} />
              <SvcRow label="NATS Broker"    status={svc(health?.services?.nats)}     icon={Radio} />
              <SvcRow label="Executor"       status={runningAgents > 0 ? 'online' : health ? 'warn' : 'dead'} icon={Network} />
              <SvcRow label="WS Gateway"     status={connected ? 'online' : 'error'} icon={Activity} />
            </div>

            {/* Execution counters */}
            <div className="panel" style={{ flexShrink: 0 }}>
              <div className="panel-header">
                <TrendingUp style={{ width: 10, height: 10, color: 'var(--cyan)' }} />
                <span style={{ letterSpacing: '0.14em' }}>EXECUTION COUNTERS</span>
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 1, background: 'var(--line-0)' }}>
                {[
                  { label: 'TOTAL EXECS',  val: totalExecs,    color: 'var(--ink-1)' },
                  { label: 'ACTIVE',       val: activeExecs,   color: activeExecs > 0 ? 'var(--lime)' : 'var(--ink-1)' },
                  { label: 'TOTAL AGENTS', val: totalAgents,   color: 'var(--ink-1)' },
                  { label: 'RUNNING',      val: runningAgents, color: runningAgents > 0 ? 'var(--lime)' : 'var(--ink-1)' },
                ].map(({ label, val, color }) => (
                  <div key={label} style={{ padding: '8px 12px', background: 'var(--surface-1)' }}>
                    <div style={{ fontSize: '8px', letterSpacing: '0.14em', color: 'var(--ink-3)', marginBottom: 3 }}>{label}</div>
                    <div style={{ fontFamily: 'var(--mono)', fontSize: '1.3rem', fontWeight: 700, color, fontVariantNumeric: 'tabular-nums' }}>
                      {val}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          </div>
        </div>

        {/* ════════════════ CHARTS 2×2 ════════════════ */}
        <div className="grid-charts" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
          <AreaPanel title="CPU UTILIZATION" id="g-cpu"
            icon={<Cpu style={{ width: 10, height: 10, color: 'var(--cyan)' }} />}
            color="var(--cyan)" dataKey="cpu" data={chart} unit="%" />
          <AreaPanel title="MEMORY USAGE" id="g-mem"
            icon={<HardDrive style={{ width: 10, height: 10, color: 'var(--lime)' }} />}
            color="var(--lime)" dataKey="mem" data={chart} unit="MB" />
          <BarPanel title="EVENT THROUGHPUT"
            icon={<Zap style={{ width: 10, height: 10, color: 'var(--amber)' }} />}
            color="var(--amber)" dataKey="ev" data={chart} />
          <BarPanel title="EXECUTION THROUGHPUT"
            icon={<Activity style={{ width: 10, height: 10, color: 'var(--violet)' }} />}
            color="var(--violet)" dataKey="ex" data={chart} />
        </div>

        {/* ════════════════ AGENT FLEET TABLE ════════════════ */}
        <div className="panel">
          <div className="panel-header">
            <Bot style={{ width: 10, height: 10, color: 'var(--cyan)' }} />
            <span style={{ letterSpacing: '0.14em' }}>AGENT FLEET</span>
            <span style={{
              marginLeft: 'auto', fontSize: '8px', padding: '2px 7px', fontWeight: 700,
              background: runningAgents > 0 ? 'rgba(57,255,20,0.07)' : 'var(--surface-0)',
              border: `1px solid ${runningAgents > 0 ? 'rgba(57,255,20,0.2)' : 'var(--line-1)'}`,
              color: runningAgents > 0 ? 'var(--lime)' : 'var(--ink-3)',
            }}>
              {runningAgents}/{totalAgents} RUNNING
            </span>
            <Link href="/agents" style={{
              marginLeft: 8, fontSize: '8px', padding: '2px 7px',
              background: 'var(--surface-0)', border: '1px solid var(--line-2)',
              color: 'var(--ink-2)', letterSpacing: '0.1em', fontWeight: 700, textDecoration: 'none',
              display: 'flex', alignItems: 'center', gap: 4,
            }}>
              MANAGE <ArrowUpRight style={{ width: 9, height: 9 }} />
            </Link>
          </div>
          <div className="table-scroll">
            <table className="op-table">
              <thead>
                <tr>
                  <th>#</th><th>AGENT ID</th><th>NAME</th><th>STATUS</th>
                  <th>CPU</th><th>MEMORY</th><th>EXECS</th>
                  <th>CONTAINER</th><th>HEARTBEAT</th><th>TYPE</th>
                </tr>
              </thead>
              <tbody>
                {loading && (
                  <tr><td colSpan={10} style={{ textAlign: 'center', padding: 28, color: 'var(--ink-3)', fontSize: 10 }}>
                    LOADING FLEET DATA...
                  </td></tr>
                )}
                {!loading && agents.length === 0 && (
                  <tr><td colSpan={10} style={{ textAlign: 'center', padding: 36, color: 'var(--ink-3)', fontSize: 10 }}>
                    <div style={{ marginBottom: 8 }}>NO AGENTS REGISTERED</div>
                    <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>
                      GO TO THE AGENTS TAB TO CREATE YOUR FIRST AGENT
                    </div>
                  </td></tr>
                )}
                {agents.map((agent, idx) => (
                  <AgentRow key={agent.id} agent={agent} live={agentLive.get(agent.id)} idx={idx} />
                ))}
              </tbody>
            </table>
          </div>
        </div>

        {/* ════════════════ LOG STREAM ════════════════ */}
        <div className="panel">
          <div className="panel-header">
            <Terminal style={{ width: 10, height: 10, color: 'var(--lime)' }} />
            <span style={{ letterSpacing: '0.14em' }}>LIVE LOG STREAM</span>
            <span style={{ marginLeft: 6, fontSize: '8.5px', color: 'var(--ink-3)' }}>
              {logs.length} ENTRIES
            </span>
            <span style={{ marginLeft: 'auto', fontSize: '8.5px', color: 'var(--ink-3)', display: 'flex', alignItems: 'center', gap: 4 }}>
              <Beacon state={connected ? 'online' : 'dead'} />
              LIVE TAIL
            </span>
          </div>
          <LogStream logs={logs} />
        </div>

      </div>

      {/* ════════ FLOATING DEPLOY BUTTON ════════ */}
      <button className="deploy-fab" onClick={() => window.location.href = '/agents'}>
        <span style={{ fontSize: 14, lineHeight: 1 }}>+</span>
        DEPLOY AGENT
      </button>

    </div>
  );
}