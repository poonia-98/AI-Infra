'use client';
import { useEffect, useState } from 'react';
import Link from 'next/link';
import { api } from '@/lib/api';
import { Clock, Play, AlertCircle, CheckCircle } from 'lucide-react';

export default function TimeTravelIndex() {
  const [executions, setExecutions] = useState<any[]>([]);
  const [agents, setAgents] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.executions.list(50).catch(() => []),
      api.agents.list(0, 100).catch(() => []),
    ]).then(([e, a]) => { setExecutions(e); setAgents(a); setLoading(false); });
  }, []);

  const agentMap = Object.fromEntries(agents.map(a => [a.id, a.name]));
  const statusIcon = (s: string) => s === 'completed' ? <CheckCircle style={{ width: 10, height: 10, color: 'var(--lime)' }} />
    : s === 'failed' ? <AlertCircle style={{ width: 10, height: 10, color: 'var(--crimson)' }} />
    : <Play style={{ width: 10, height: 10, color: 'var(--cyan)' }} />;

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
          <Clock style={{ width: 18, height: 18, color: 'var(--cyan)' }} />
          <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>
            TIME TRAVEL DEBUGGER
          </div>
        </div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>
          EXECUTION REPLAY · STEP INSPECTION · PROMPT HISTORY · DECISION GRAPH
        </div>
      </div>

      {/* Capabilities banner */}
      <div style={{
        display: 'flex', gap: 12, marginBottom: 20, flexWrap: 'wrap',
      }}>
        {['REWIND EXECUTION', 'INSPECT EVERY STEP', 'EDIT & REPLAY PROMPTS', 'TRACK TOOL CALLS', 'DECISION GRAPH'].map(cap => (
          <div key={cap} style={{
            padding: '4px 10px', fontSize: 8, letterSpacing: '0.12em',
            color: 'var(--cyan)', border: '1px solid rgba(0,217,255,0.2)',
            background: 'rgba(0,217,255,0.05)',
          }}>
            {cap}
          </div>
        ))}
      </div>

      <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
        <div style={{ padding: '8px 16px', borderBottom: '1px solid var(--line-1)', display: 'flex', justifyContent: 'space-between' }}>
          <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>SELECT EXECUTION TO DEBUG</span>
          <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{executions.length} EXECUTIONS</span>
        </div>

        {loading ? (
          <div style={{ padding: 40, textAlign: 'center', fontSize: 9, color: 'var(--ink-4)' }}>LOADING EXECUTIONS...</div>
        ) : executions.length === 0 ? (
          <div style={{ padding: 40, textAlign: 'center' }}>
            <div style={{ fontSize: 10, color: 'var(--ink-4)', letterSpacing: '0.12em', marginBottom: 8 }}>NO EXECUTIONS RECORDED</div>
            <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>Run agents to generate execution history for time travel debugging</div>
          </div>
        ) : (
          <div>
            {/* Header row */}
            <div style={{
              display: 'grid', gridTemplateColumns: '2fr 1.5fr 80px 80px 80px 100px',
              padding: '6px 16px', borderBottom: '1px solid var(--line-0)',
              fontSize: 8, color: 'var(--ink-4)', letterSpacing: '0.12em',
            }}>
              <span>EXECUTION ID</span>
              <span>AGENT</span>
              <span>STATUS</span>
              <span>STEPS</span>
              <span>DURATION</span>
              <span>STARTED</span>
            </div>
            {executions.map(ex => (
              <Link
                key={ex.id}
                href={`/time-travel/${ex.agent_id}?execution=${ex.id}`}
                style={{ textDecoration: 'none' }}
              >
                <div style={{
                  display: 'grid', gridTemplateColumns: '2fr 1.5fr 80px 80px 80px 100px',
                  padding: '9px 16px', borderBottom: '1px solid var(--line-0)',
                  cursor: 'pointer', transition: 'background 0.1s',
                }}
                  onMouseEnter={e => (e.currentTarget as HTMLDivElement).style.background = 'rgba(0,217,255,0.04)'}
                  onMouseLeave={e => (e.currentTarget as HTMLDivElement).style.background = 'transparent'}
                >
                  <span style={{ fontSize: 9.5, fontFamily: 'monospace', color: 'var(--cyan)' }}>
                    {ex.id?.slice(0, 20)}...
                  </span>
                  <span style={{ fontSize: 9, color: 'var(--ink-1)' }}>
                    {agentMap[ex.agent_id] || ex.agent_id?.slice(0, 12)}
                  </span>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                    {statusIcon(ex.status)}
                    <span style={{ fontSize: 8.5, color: ex.status === 'completed' ? 'var(--lime)' : ex.status === 'failed' ? 'var(--crimson)' : 'var(--cyan)' }}>
                      {ex.status?.toUpperCase()}
                    </span>
                  </div>
                  <span style={{ fontSize: 9, color: 'var(--ink-2)' }}>{ex.total_steps ?? '—'}</span>
                  <span style={{ fontSize: 9, color: 'var(--ink-2)' }}>
                    {ex.duration_ms ? `${ex.duration_ms}ms` : '—'}
                  </span>
                  <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>
                    {ex.started_at?.slice(11, 19) || '—'}
                  </span>
                </div>
              </Link>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}