'use client';
import { useEffect, useState } from 'react';
import { api, EvolutionExperiment } from '@/lib/api';

export default function EvolutionPage() {
  const [experiments, setExperiments] = useState<EvolutionExperiment[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.evolution.experiments().catch(() => []).then(e => { setExperiments(e); setLoading(false); });
  }, []);

  const statusColor = (s: string) => s === 'running' ? 'var(--lime)' : s === 'completed' ? 'var(--cyan)' : s === 'failed' ? 'var(--crimson)' : 'var(--ink-4)';

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>EVOLUTION ENGINE</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>GENETIC ALGORITHMS · AGENT MUTATION · FITNESS TRACKING</div>
      </div>
      {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>LOADING EXPERIMENTS...</div>
        : experiments.length === 0 ? (
          <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)', padding: 40, textAlign: 'center' }}>
            <div style={{ fontSize: 10, color: 'var(--ink-4)', letterSpacing: '0.12em', marginBottom: 8 }}>NO EVOLUTION EXPERIMENTS</div>
            <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>Create experiments via API to begin agent evolution cycles</div>
          </div>
        ) : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 14 }}>
            {experiments.map(exp => (
              <div key={exp.id} style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)', padding: 16 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 10 }}>
                  <div style={{ fontFamily: 'var(--display)', fontSize: 13, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.06em' }}>{exp.name}</div>
                  <div style={{ fontSize: 8, color: statusColor(exp.status), letterSpacing: '0.1em' }}>{exp.status.toUpperCase()}</div>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 8, marginBottom: 10 }}>
                  {[
                    { label: 'GENERATION', value: exp.generation, color: 'var(--cyan)' },
                    { label: 'POPULATION', value: exp.population_size, color: 'var(--violet)' },
                    { label: 'BEST FIT', value: exp.best_fitness?.toFixed(3) ?? '—', color: 'var(--lime)' },
                  ].map(({ label, value, color }) => (
                    <div key={label} style={{ textAlign: 'center', background: 'var(--surface-1)', padding: '6px 4px' }}>
                      <div style={{ fontSize: 13, fontWeight: 700, color }}>{value}</div>
                      <div style={{ fontSize: 7, color: 'var(--ink-4)', letterSpacing: '0.1em' }}>{label}</div>
                    </div>
                  ))}
                </div>
                <div style={{ fontSize: 8, color: 'var(--ink-4)' }}>Started: {exp.created_at?.slice(0, 16)}</div>
              </div>
            ))}
          </div>
        )}
    </div>
  );
}