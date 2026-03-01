'use client';
import { useEffect, useState } from 'react';
import { api } from '@/lib/api';
import { Lock, RotateCcw, Eye, EyeOff } from 'lucide-react';

export default function VaultPage() {
  const [secrets, setSecrets] = useState<any[]>([]);
  const [keys, setKeys] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [rotating, setRotating] = useState<string | null>(null);
  const [revealed, setRevealed] = useState<Set<string>>(new Set());

  useEffect(() => {
    Promise.all([
      api.vault.secrets().catch(() => []),
      api.vault.keys().catch(() => []),
    ]).then(([s, k]) => { setSecrets(s); setKeys(k); setLoading(false); });
  }, []);

  async function rotate(id: string) {
    setRotating(id);
    try { await api.vault.rotateKey(id); } catch { /* show toast */ }
    setRotating(null);
  }

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>SECRETS VAULT</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>AES-256-GCM ENCRYPTED · KEY ROTATION · ACCESS LOGS</div>
      </div>

      {/* Encryption status banner */}
      <div style={{
        padding: '8px 14px', marginBottom: 20,
        background: 'rgba(57,255,20,0.05)', border: '1px solid rgba(57,255,20,0.15)',
        display: 'flex', alignItems: 'center', gap: 10,
      }}>
        <div style={{ width: 6, height: 6, borderRadius: '50%', background: 'var(--lime)', animation: 'breathe-green 2s ease-in-out infinite' }} />
        <span style={{ fontSize: 9, color: 'var(--lime)', letterSpacing: '0.1em' }}>
          VAULT SEALED · AES-256-GCM · ENVELOPE ENCRYPTION ACTIVE
        </span>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
        {/* Secrets */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)', display: 'flex', justifyContent: 'space-between' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>SECRETS</span>
            <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{secrets.length} stored</span>
          </div>
          <div style={{ padding: '8px 14px' }}>
            {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)', padding: 16 }}>LOADING...</div>
              : secrets.length === 0 ? (
                <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>No secrets in vault</div>
              ) : secrets.map(s => (
                <div key={s.id} style={{ padding: '8px 0', borderBottom: '1px solid var(--line-0)' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 3 }}>
                    <span style={{ fontSize: 10, color: 'var(--ink-0)', fontWeight: 600 }}>{s.name}</span>
                    <div style={{ display: 'flex', gap: 6 }}>
                      <button
                        onClick={() => setRevealed(prev => { const n = new Set(prev); n.has(s.id) ? n.delete(s.id) : n.add(s.id); return n; })}
                        style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--ink-3)', padding: 2 }}
                      >
                        {revealed.has(s.id) ? <EyeOff style={{ width: 10, height: 10 }} /> : <Eye style={{ width: 10, height: 10 }} />}
                      </button>
                      <button
                        onClick={() => rotate(s.id)}
                        disabled={rotating === s.id}
                        style={{ background: 'none', border: 'none', cursor: 'pointer', color: rotating === s.id ? 'var(--amber)' : 'var(--ink-3)', padding: 2 }}
                      >
                        <RotateCcw style={{ width: 10, height: 10 }} />
                      </button>
                    </div>
                  </div>
                  <div style={{ fontSize: 8.5, color: 'var(--ink-4)', fontFamily: 'monospace' }}>
                    {revealed.has(s.id) ? (s.value_preview || '••••••') : '••••••••••••••••'}
                  </div>
                  <div style={{ fontSize: 8, color: 'var(--ink-4)', marginTop: 2 }}>
                    v{s.version || 1} · Updated {s.updated_at?.slice(0, 10) || s.created_at?.slice(0, 10)}
                  </div>
                </div>
              ))}
          </div>
        </div>

        {/* Encryption keys */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--amber)', fontWeight: 700 }}>ENCRYPTION KEYS</span>
          </div>
          <div style={{ padding: '8px 14px' }}>
            {keys.length === 0 ? (
              <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 0' }}>No key metadata available</div>
            ) : keys.map((k, i) => (
              <div key={i} style={{ padding: '8px 0', borderBottom: '1px solid var(--line-0)' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }}>
                  <span style={{ fontSize: 9.5, color: 'var(--ink-0)', fontWeight: 600 }}>{k.key_id?.slice(0, 12)}...</span>
                  <span style={{ fontSize: 8, color: k.active ? 'var(--lime)' : 'var(--ink-4)', letterSpacing: '0.08em' }}>
                    {k.active ? 'ACTIVE' : 'RETIRED'}
                  </span>
                </div>
                <div style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>
                  {k.algorithm} · Created {k.created_at?.slice(0, 10)}
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}