'use client';
import { useEffect, useState } from 'react';
import { api } from '@/lib/api';

export default function ObservabilityPage() {
  const [traces, setTraces] = useState<any[]>([]);
  const [performance, setPerformance] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.observability.traces(undefined, 20).catch(() => []),
      api.observability.performance(undefined, '1h').catch(() => []),
    ]).then(([t, p]) => { setTraces(t); setPerformance(p); setLoading(false); });
  }, []);

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>OBSERVABILITY</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>TRACES · LINEAGE · PERFORMANCE · DIAGNOSTICS</div>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
        {/* Traces */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)', display: 'flex', justifyContent: 'space-between' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>EXECUTION TRACES</span>
            <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>LAST 20</span>
          </div>
          <div style={{ padding: '8px 14px', maxHeight: 400, overflowY: 'auto' }}>
            {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)', padding: '16px 0' }}>LOADING...</div>
              : traces.length === 0 ? (
                <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>
                  No traces — run agents to see execution traces
                </div>
              ) : traces.map((trace, i) => (
                <div key={i} style={{ padding: '7px 0', borderBottom: '1px solid var(--line-0)', display: 'flex', gap: 10 }}>
                  <div style={{
                    width: 4, flexShrink: 0, alignSelf: 'stretch',
                    background: trace.status === 'success' ? 'var(--lime)' : trace.status === 'error' ? 'var(--crimson)' : 'var(--cyan)',
                  }} />
                  <div style={{ flex: 1 }}>
                    <div style={{ fontSize: 9.5, color: 'var(--ink-1)', letterSpacing: '0.04em' }}>
                      {trace.agent_id?.slice(0, 8) || 'unknown'}
                    </div>
                    <div style={{ fontSize: 8.5, color: 'var(--ink-4)', display: 'flex', gap: 10, marginTop: 2 }}>
                      <span>{trace.duration_ms || 0}ms</span>
                      <span>{trace.span_count || 0} spans</span>
                      <span>{trace.created_at?.slice(11, 19)}</span>
                    </div>
                  </div>
                </div>
              ))}
          </div>
        </div>

        {/* Performance */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)', display: 'flex', justifyContent: 'space-between' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--violet)', fontWeight: 700 }}>PERFORMANCE SNAPSHOTS</span>
            <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>1H</span>
          </div>
          <div style={{ padding: '8px 14px', maxHeight: 400, overflowY: 'auto' }}>
            {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)', padding: '16px 0' }}>LOADING...</div>
              : performance.length === 0 ? (
                <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>
                  No performance data in this window
                </div>
              ) : performance.map((snap, i) => (
                <div key={i} style={{ padding: '7px 0', borderBottom: '1px solid var(--line-0)' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 4 }}>
                    <span style={{ fontSize: 9.5, color: 'var(--ink-1)' }}>{snap.agent_id?.slice(0, 8)}</span>
                    <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{snap.timestamp?.slice(11, 19)}</span>
                  </div>
                  <div style={{ display: 'flex', gap: 12 }}>
                    {[
                      { label: 'CPU', value: snap.cpu_percent, color: 'var(--cyan)' },
                      { label: 'MEM', value: snap.memory_mb, color: 'var(--lime)' },
                    ].map(({ label, value, color }) => (
                      <div key={label} style={{ flex: 1 }}>
                        <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 2 }}>
                          <span style={{ fontSize: 8, color: 'var(--ink-4)' }}>{label}</span>
                          <span style={{ fontSize: 8, color }}>{value || 0}</span>
                        </div>
                        <div style={{ height: 2, background: 'var(--line-1)' }}>
                          <div style={{ height: '100%', width: `${Math.min(100, (value || 0) / 100 * 100)}%`, background: color }} />
                        </div>
                      </div>
                    ))}
                  </div>
                </div>
              ))}
          </div>
        </div>
      </div>
    </div>
  );
}