'use client';
import { useEffect, useState } from 'react';
import { api, SandboxPolicy } from '@/lib/api';
import { Shield } from 'lucide-react';

export default function SandboxPage() {
  const [policies, setPolicies] = useState<SandboxPolicy[]>([]);
  const [violations, setViolations] = useState<any[]>([]);
  const [runtimes, setRuntimes] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.sandbox.policies().catch(() => []),
      api.sandbox.violations(20).catch(() => []),
      api.sandbox.runtimes().catch(() => ({ runtimes: ['docker', 'gvisor', 'firecracker'] })),
    ]).then(([p, v, r]) => {
      setPolicies(p); setViolations(v); setRuntimes(r.runtimes || []); setLoading(false);
    });
  }, []);

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>SANDBOX MANAGER</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>ISOLATION POLICIES · RUNTIMES · VIOLATIONS</div>
      </div>

      {/* Available runtimes */}
      <div style={{ display: 'flex', gap: 10, marginBottom: 20 }}>
        {runtimes.map(rt => (
          <div key={rt} style={{
            padding: '6px 14px', background: 'var(--surface-1)',
            border: '1px solid var(--line-2)', fontSize: 9.5,
            color: 'var(--cyan)', letterSpacing: '0.1em', fontWeight: 700,
          }}>
            {rt.toUpperCase()}
          </div>
        ))}
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
        {/* Policies */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>ISOLATION POLICIES</span>
            <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{policies.length} active</span>
          </div>
          <div style={{ padding: '8px 14px' }}>
            {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)', padding: '16px 0' }}>LOADING...</div>
              : policies.length === 0 ? (
                <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>No sandbox policies defined</div>
              ) : policies.map(p => (
                <div key={p.id} style={{ padding: '8px 0', borderBottom: '1px solid var(--line-0)' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }}>
                    <span style={{ fontSize: 10, color: 'var(--ink-0)', fontWeight: 600 }}>{p.policy_name}</span>
                    <span style={{ fontSize: 8, background: p.enabled ? 'var(--lime-dim)' : 'var(--crimson-dim)', color: p.enabled ? 'var(--lime)' : 'var(--crimson)', padding: '1px 6px', letterSpacing: '0.08em' }}>
                      {p.enabled ? 'ACTIVE' : 'DISABLED'}
                    </span>
                  </div>
                  <div style={{ fontSize: 8.5, color: 'var(--ink-4)', display: 'flex', gap: 12 }}>
                    <span>{p.runtime.toUpperCase()}</span>
                    <span>{p.max_cpu_cores} CPU</span>
                    <span>{p.max_memory_mb}MB RAM</span>
                    <span style={{ color: p.network_enabled ? 'var(--amber)' : 'var(--lime)' }}>
                      NET:{p.network_enabled ? 'ON' : 'OFF'}
                    </span>
                  </div>
                </div>
              ))}
          </div>
        </div>

        {/* Violations */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--crimson)', fontWeight: 700 }}>SECURITY VIOLATIONS</span>
          </div>
          <div style={{ padding: '8px 14px' }}>
            {violations.length === 0 ? (
              <div style={{ fontSize: 9, color: 'var(--lime)', textAlign: 'center', padding: '32px 0' }}>
                ✓ No violations detected
              </div>
            ) : violations.map((v, i) => (
              <div key={i} style={{ padding: '7px 0', borderBottom: '1px solid var(--line-0)' }}>
                <div style={{ fontSize: 9.5, color: 'var(--crimson)' }}>{v.violation_type}</div>
                <div style={{ fontSize: 8.5, color: 'var(--ink-4)', marginTop: 2 }}>{v.created_at?.slice(0, 19)}</div>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}