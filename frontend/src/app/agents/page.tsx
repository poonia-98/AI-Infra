'use client';
import { useEffect, useState, useCallback } from 'react';
import Link from 'next/link';
import {
  Plus, Play, Square, RotateCcw, Trash2, Search, Bot,
  ChevronRight, AlertCircle, Clock, Cpu, Layers,
} from 'lucide-react';
import { api, Agent } from '@/lib/api';
import CreateAgentModal from '@/components/CreateAgentModal';
import { formatDistanceToNow, format } from 'date-fns';
import { useWebSocket } from '@/hooks/useWebSocket';
import clsx from 'clsx';

const STATUS_TAG: Record<string, string> = {
  running: 'tag-running', stopped: 'tag-stopped', created: 'tag-created',
  error: 'tag-error', failed: 'tag-failed', idle: 'tag-idle',
  pending: 'tag-pending', completed: 'tag-completed',
};

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

export default function AgentsPage() {
  const [agents, setAgents]   = useState<Agent[]>([]);
  const [loading, setLoading] = useState(true);
  const [search, setSearch]   = useState('');
  const [filter, setFilter]   = useState<string>('all');
  const [showCreate, setShowCreate] = useState(false);
  const [busy, setBusy]       = useState<string | null>(null);
  const [toast, setToast]     = useState<string | null>(null);
  const { subscribe } = useWebSocket();

  const showToast = (msg: string) => {
    setToast(msg);
    setTimeout(() => setToast(null), 3000);
  };

  const fetchAgents = useCallback(async () => {
    try { setAgents(await api.agents.list()); }
    catch { /* noop */ }
    finally { setLoading(false); }
  }, []);

  useEffect(() => {
    fetchAgents();
    const id = setInterval(fetchAgents, 10000);
    return () => clearInterval(id);
  }, [fetchAgents]);

  useEffect(() => {
    return subscribe('agents.events', () => {
      setTimeout(fetchAgents, 500);
    });
  }, [subscribe, fetchAgents]);

  async function act(id: string, action: 'start'|'stop'|'restart'|'delete') {
    setBusy(`${id}-${action}`);
    try {
      action === 'delete' ? await api.agents.delete(id) : await api.agents[action](id);
      await fetchAgents();
      showToast(`Agent ${action} successful`);
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Action failed');
    } finally { setBusy(null); }
  }

  const counts = agents.reduce<Record<string, number>>((acc, a) => {
    acc[a.status] = (acc[a.status] || 0) + 1;
    return acc;
  }, {});

  const filtered = agents.filter(a => {
    const matchFilter = filter === 'all' || a.status === filter;
    const matchSearch = !search ||
      a.name.toLowerCase().includes(search.toLowerCase()) ||
      a.id.toLowerCase().includes(search.toLowerCase()) ||
      a.agent_type.toLowerCase().includes(search.toLowerCase());
    return matchFilter && matchSearch;
  });

  const FILTERS = [
    { key: 'all', label: 'ALL', count: agents.length },
    { key: 'running', label: 'RUNNING', count: counts.running || 0 },
    { key: 'stopped', label: 'STOPPED', count: counts.stopped || 0 },
    { key: 'created', label: 'CREATED', count: counts.created || 0 },
    { key: 'error',   label: 'ERROR',   count: (counts.error || 0) + (counts.failed || 0) },
  ];

  return (
    <div style={{ height: '100%', overflowY: 'auto', background: 'var(--base)' }}>

      {/* Toast */}
      {toast && (
        <div style={{
          position: 'fixed', bottom: 24, right: 24, zIndex: 100,
          background: 'var(--surface-2)', border: '1px solid var(--line-2)',
          padding: '10px 16px', fontSize: 11, color: 'var(--ink-0)',
          boxShadow: '0 8px 32px rgba(0,0,0,0.5)',
          animation: 'slide-in-top 0.2s ease-out',
        }}>
          {toast}
        </div>
      )}

      {/* Header */}
      <div style={{
        position: 'sticky', top: 0, zIndex: 40,
        background: 'rgba(3,7,15,0.97)', backdropFilter: 'blur(20px)',
        borderBottom: '1px solid var(--line-0)',
      }}>
        <div style={{ height: 1, background: 'linear-gradient(90deg, var(--cyan), var(--lime))', opacity: 0.4 }} />
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '8px 16px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <Bot style={{ width: 14, height: 14, color: 'var(--cyan)' }} />
            <div style={{ fontFamily: 'var(--display)', fontSize: 15, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.10em' }}>
              AGENT FLEET
            </div>
            <ChevronRight style={{ width: 12, height: 12, color: 'var(--ink-3)' }} />
            <div style={{ fontSize: 9, color: 'var(--ink-3)', letterSpacing: '0.12em', fontFamily: 'var(--mono)' }}>
              {format(new Date(), 'yyyy-MM-dd HH:mm:ss')}
            </div>
          </div>
          <button
            onClick={() => setShowCreate(true)}
            className="op-btn op-btn-lime"
          >
            <Plus style={{ width: 11, height: 11 }} />
            DEPLOY AGENT
          </button>
        </div>
      </div>

      <div style={{ padding: 16 }}>

        {/* Stats bar */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 8, marginBottom: 12 }}>
          {[
            { label: 'TOTAL AGENTS',   value: agents.length,              color: 'var(--cyan)',    icon: Bot },
            { label: 'RUNNING',        value: counts.running  || 0,       color: 'var(--lime)',    icon: Play },
            { label: 'STOPPED',        value: counts.stopped  || 0,       color: 'var(--ink-2)',   icon: Square },
            { label: 'FAILED',         value: (counts.error || 0) + (counts.failed || 0), color: 'var(--crimson)', icon: AlertCircle },
          ].map(({ label, value, color, icon: Icon }) => (
            <div key={label} className="panel" style={{ padding: '12px 14px', display: 'flex', alignItems: 'center', gap: 12 }}>
              <div style={{
                width: 32, height: 32, display: 'flex', alignItems: 'center', justifyContent: 'center',
                background: `${color}10`, border: `1px solid ${color}25`,
              }}>
                <Icon style={{ width: 14, height: 14, color }} />
              </div>
              <div>
                <div style={{ fontSize: 8.5, letterSpacing: '0.12em', color: 'var(--ink-3)', fontWeight: 700, marginBottom: 2 }}>{label}</div>
                <div style={{ fontFamily: 'var(--mono)', fontSize: '1.4rem', fontWeight: 700, color, fontVariantNumeric: 'tabular-nums', lineHeight: 1 }}>
                  {value}
                </div>
              </div>
            </div>
          ))}
        </div>

        {/* Filter + search bar */}
        <div className="panel" style={{ marginBottom: 10 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 0, borderBottom: '1px solid var(--line-0)' }}>
            {FILTERS.map(f => (
              <button
                key={f.key}
                onClick={() => setFilter(f.key)}
                style={{
                  padding: '6px 14px',
                  background: filter === f.key ? 'rgba(0,217,255,0.08)' : 'transparent',
                  border: 'none',
                  borderBottom: filter === f.key ? '2px solid var(--cyan)' : '2px solid transparent',
                  color: filter === f.key ? 'var(--cyan)' : 'var(--ink-3)',
                  fontFamily: 'var(--mono)', fontSize: 9, fontWeight: 700, letterSpacing: '0.10em',
                  cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 6,
                  transition: 'all 0.15s',
                }}
              >
                {f.label}
                {f.count > 0 && (
                  <span style={{
                    fontSize: 8, padding: '1px 4px',
                    background: filter === f.key ? 'rgba(0,217,255,0.15)' : 'var(--surface-0)',
                    border: `1px solid ${filter === f.key ? 'rgba(0,217,255,0.2)' : 'var(--line-1)'}`,
                    color: filter === f.key ? 'var(--cyan)' : 'var(--ink-3)',
                  }}>
                    {f.count}
                  </span>
                )}
              </button>
            ))}
            <div style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', padding: '4px 10px', gap: 6 }}>
              <Search style={{ width: 10, height: 10, color: 'var(--ink-3)' }} />
              <input
                className="op-input"
                placeholder="SEARCH AGENTS..."
                value={search}
                onChange={e => setSearch(e.target.value)}
                style={{ width: 200, border: 'none', background: 'transparent', padding: '2px 0' }}
              />
            </div>
          </div>

          {/* Table */}
          <div style={{ overflowX: 'auto' }}>
            <table className="op-table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>AGENT ID</th>
                  <th>NAME</th>
                  <th>TYPE</th>
                  <th>STATUS</th>
                  <th>IMAGE</th>
                  <th>CONTAINER</th>
                  <th>MEMORY</th>
                  <th>CREATED</th>
                  <th>UPDATED</th>
                  <th style={{ textAlign: 'right' }}>ACTIONS</th>
                </tr>
              </thead>
              <tbody>
                {loading && (
                  <tr><td colSpan={11} style={{ textAlign: 'center', padding: 32, color: 'var(--ink-3)', fontSize: 10 }}>
                    LOADING AGENT FLEET...
                  </td></tr>
                )}
                {!loading && filtered.length === 0 && (
                  <tr><td colSpan={11} style={{ textAlign: 'center', padding: 40, color: 'var(--ink-3)' }}>
                    <div style={{ marginBottom: 8, fontSize: 10, letterSpacing: '0.1em' }}>NO AGENTS FOUND</div>
                    <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>
                      {agents.length === 0 ? 'DEPLOY YOUR FIRST AGENT TO GET STARTED' : 'TRY ADJUSTING YOUR SEARCH OR FILTER'}
                    </div>
                  </td></tr>
                )}
                {filtered.map((agent, idx) => {
                  const isRunning = agent.status === 'running';
                  return (
                    <tr key={agent.id}>
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
                        <Link href={`/agents/${agent.id}`} style={{ color: 'var(--ink-0)', fontWeight: 600, fontSize: 11.5 }}>
                          {agent.name}
                        </Link>
                        {agent.description && (
                          <div style={{ fontSize: 9, color: 'var(--ink-3)', marginTop: 1 }}>
                            {agent.description.slice(0, 40)}{agent.description.length > 40 ? '…' : ''}
                          </div>
                        )}
                      </td>
                      <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-2)' }}>
                        {agent.agent_type}
                      </td>
                      <td>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                          {isRunning && <Beacon state="online" />}
                          <span className={`tag ${STATUS_TAG[agent.status] ?? 'tag-stopped'}`}>
                            {agent.status}
                          </span>
                        </div>
                      </td>
                      <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)', maxWidth: 160 }}>
                        <span style={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {agent.image}
                        </span>
                      </td>
                      <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)' }}>
                        {agent.container_id ? agent.container_id.slice(0, 12) : '—'}
                      </td>
                      <td style={{ fontFamily: 'var(--mono)', fontSize: 9.5, color: 'var(--ink-2)' }}>
                        {agent.memory_limit}
                      </td>
                      <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)', whiteSpace: 'nowrap' }}>
                        {formatDistanceToNow(new Date(agent.created_at), { addSuffix: true })}
                      </td>
                      <td style={{ fontFamily: 'var(--mono)', fontSize: 9, color: 'var(--ink-3)', whiteSpace: 'nowrap' }}>
                        {formatDistanceToNow(new Date(agent.updated_at), { addSuffix: true })}
                      </td>
                      <td>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 4, justifyContent: 'flex-end' }}>
                          {agent.status !== 'running' && (
                            <button
                              className="op-btn op-btn-lime"
                              style={{ padding: '3px 8px', fontSize: 8 }}
                              disabled={busy === `${agent.id}-start`}
                              onClick={() => act(agent.id, 'start')}
                            >
                              <Play style={{ width: 9, height: 9 }} />
                              {busy === `${agent.id}-start` ? '...' : 'START'}
                            </button>
                          )}
                          {agent.status === 'running' && (
                            <>
                              <button
                                className="op-btn op-btn-crimson"
                                style={{ padding: '3px 8px', fontSize: 8 }}
                                disabled={busy === `${agent.id}-stop`}
                                onClick={() => act(agent.id, 'stop')}
                              >
                                <Square style={{ width: 9, height: 9 }} />
                                {busy === `${agent.id}-stop` ? '...' : 'STOP'}
                              </button>
                              <button
                                className="op-btn op-btn-cyan"
                                style={{ padding: '3px 8px', fontSize: 8 }}
                                disabled={busy === `${agent.id}-restart`}
                                onClick={() => act(agent.id, 'restart')}
                              >
                                <RotateCcw style={{ width: 9, height: 9 }} />
                                {busy === `${agent.id}-restart` ? '...' : 'RESTART'}
                              </button>
                            </>
                          )}
                          <button
                            className="op-btn op-btn-ghost"
                            style={{ padding: '3px 8px', fontSize: 8 }}
                            disabled={busy === `${agent.id}-delete` || agent.status === 'running'}
                            onClick={() => { if (confirm('Delete this agent?')) act(agent.id, 'delete'); }}
                          >
                            <Trash2 style={{ width: 9, height: 9 }} />
                          </button>
                          <Link
                            href={`/agents/${agent.id}`}
                            style={{
                              display: 'flex', alignItems: 'center', gap: 4,
                              padding: '3px 8px', fontSize: 8,
                              border: '1px solid var(--line-2)', color: 'var(--ink-2)',
                              letterSpacing: '0.1em', fontWeight: 700, textDecoration: 'none',
                              fontFamily: 'var(--mono)',
                            }}
                          >
                            VIEW
                          </Link>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      </div>

      {showCreate && (
        <CreateAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={() => { setShowCreate(false); fetchAgents(); showToast('Agent deployed'); }}
        />
      )}
    </div>
  );
}