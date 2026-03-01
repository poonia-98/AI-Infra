'use client';
import { useEffect, useState } from 'react';
import { api, SimulationEnvironment } from '@/lib/api';

export default function SimulationPage() {
  const [envs, setEnvs] = useState<SimulationEnvironment[]>([]);
  const [loading, setLoading] = useState(true);
  const [starting, setStarting] = useState<string | null>(null);

  useEffect(() => {
    api.simulation.environments().catch(() => []).then(e => { setEnvs(e); setLoading(false); });
  }, []);

  async function startRun(envId: string) {
    setStarting(envId);
    try { await api.simulation.startRun(envId); } catch { /* handle */ }
    setStarting(null);
  }

  const statusColor = (s: string) => s === 'running' ? 'var(--lime)' : s === 'error' ? 'var(--crimson)' : s === 'idle' ? 'var(--cyan)' : 'var(--ink-4)';

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>SIMULATION ENGINE</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>SYNTHETIC WORKLOADS · CHAOS TESTING · BEHAVIOR ANALYSIS</div>
      </div>

      {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>LOADING ENVIRONMENTS...</div>
        : envs.length === 0 ? (
          <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)', padding: 40, textAlign: 'center' }}>
            <div style={{ fontSize: 10, color: 'var(--ink-4)', letterSpacing: '0.12em', marginBottom: 8 }}>NO SIMULATION ENVIRONMENTS</div>
            <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>Create environments via API to run synthetic workload tests</div>
          </div>
        ) : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 14 }}>
            {envs.map(env => (
              <div key={env.id} style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)', padding: 16 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 8 }}>
                  <div style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.06em' }}>
                    {env.name}
                  </div>
                  <div style={{ fontSize: 8, color: statusColor(env.status), letterSpacing: '0.1em', fontWeight: 700 }}>
                    {env.status.toUpperCase()}
                  </div>
                </div>
                <div style={{ fontSize: 9, color: 'var(--ink-3)', marginBottom: 10, lineHeight: 1.6 }}>{env.description}</div>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  <span style={{ fontSize: 8.5, color: 'var(--ink-4)', background: 'var(--surface-2)', padding: '2px 8px', letterSpacing: '0.08em' }}>
                    {env.scenario_type}
                  </span>
                  <button
                    onClick={() => startRun(env.id)}
                    disabled={starting === env.id}
                    style={{
                      padding: '4px 12px', background: 'rgba(57,255,20,0.08)',
                      border: '1px solid rgba(57,255,20,0.3)', color: 'var(--lime)',
                      fontFamily: 'var(--mono)', fontSize: 8.5, cursor: 'pointer',
                      letterSpacing: '0.08em',
                    }}
                  >
                    {starting === env.id ? '◌ STARTING' : '▶ RUN'}
                  </button>
                </div>
              </div>
            ))}
          </div>
        )}
    </div>
  );
}