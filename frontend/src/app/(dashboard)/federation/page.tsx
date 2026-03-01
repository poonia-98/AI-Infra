'use client';
import { useEffect, useState } from 'react';
import { api, FederationPeer } from '@/lib/api';

export default function FederationPage() {
  const [peers, setPeers] = useState<FederationPeer[]>([]);
  const [tasks, setTasks] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.federation.peers().catch(() => []),
      api.federation.tasks(20).catch(() => []),
    ]).then(([p, t]) => { setPeers(p); setTasks(t); setLoading(false); });
  }, []);

  const trustColor = (t: string) => t === 'full' ? 'var(--lime)' : t === 'partial' ? 'var(--amber)' : 'var(--ink-4)';

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>FEDERATION NETWORK</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>CROSS-ORG PEERS · TASK DISPATCH · TRUST ZONES</div>
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)', display: 'flex', justifyContent: 'space-between' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>FEDERATION PEERS</span>
            <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{peers.length}</span>
          </div>
          <div style={{ padding: '8px 14px' }}>
            {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)', padding: 16 }}>LOADING...</div>
              : peers.length === 0 ? <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>No federation peers configured</div>
              : peers.map(peer => (
                <div key={peer.id} style={{ padding: '8px 0', borderBottom: '1px solid var(--line-0)' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }}>
                    <span style={{ fontSize: 10, color: 'var(--ink-0)', fontWeight: 600 }}>{peer.peer_name}</span>
                    <span style={{ fontSize: 8, color: peer.status === 'active' ? 'var(--lime)' : 'var(--crimson)', letterSpacing: '0.08em' }}>
                      {peer.status.toUpperCase()}
                    </span>
                  </div>
                  <div style={{ fontSize: 8.5, color: 'var(--ink-4)', display: 'flex', gap: 12 }}>
                    <span style={{ color: trustColor(peer.trust_level) }}>TRUST:{peer.trust_level.toUpperCase()}</span>
                    <span>{peer.endpoint_url?.slice(0, 30)}</span>
                  </div>
                </div>
              ))}
          </div>
        </div>
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--violet)', fontWeight: 700 }}>FEDERATED TASKS</span>
          </div>
          <div style={{ padding: '8px 14px' }}>
            {tasks.length === 0 ? <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>No federated tasks dispatched</div>
              : tasks.map((t, i) => (
                <div key={i} style={{ padding: '7px 0', borderBottom: '1px solid var(--line-0)', display: 'flex', justifyContent: 'space-between' }}>
                  <div>
                    <div style={{ fontSize: 9.5, color: 'var(--ink-1)' }}>{t.task_type || 'task'}</div>
                    <div style={{ fontSize: 8, color: 'var(--ink-4)' }}>{t.created_at?.slice(0, 16)}</div>
                  </div>
                  <span style={{ fontSize: 8, color: t.status === 'completed' ? 'var(--lime)' : t.status === 'failed' ? 'var(--crimson)' : 'var(--amber)', letterSpacing: '0.08em', alignSelf: 'center' }}>
                    {(t.status || 'pending').toUpperCase()}
                  </span>
                </div>
              ))}
          </div>
        </div>
      </div>
    </div>
  );
}