'use client';
import { useEffect, useState } from 'react';
import { api, Region } from '@/lib/api';
import { Globe } from 'lucide-react';

export default function RegionsPage() {
  const [regions, setRegions] = useState<Region[]>([]);
  const [placements, setPlacements] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.regions.list().catch(() => []),
      api.regions.placements(20).catch(() => []),
    ]).then(([r, p]) => { setRegions(r); setPlacements(p); setLoading(false); });
  }, []);

  const statusColor = (s: string) => s === 'active' ? 'var(--lime)' : s === 'degraded' ? 'var(--amber)' : 'var(--crimson)';

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>GLOBAL REGIONS</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>MULTI-REGION · PLACEMENT · FAILOVER · CLUSTERS</div>
      </div>

      {/* Region cards */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(240px, 1fr))', gap: 14, marginBottom: 24 }}>
        {loading ? Array.from({ length: 3 }).map((_, i) => (
          <div key={i} className="skeleton" style={{ height: 120 }} />
        )) : regions.length === 0 ? (
          <div style={{ gridColumn: '1/-1', background: 'var(--surface-0)', border: '1px solid var(--line-1)', padding: 40, textAlign: 'center' }}>
            <div style={{ fontSize: 10, color: 'var(--ink-4)', letterSpacing: '0.12em' }}>NO REGIONS CONFIGURED</div>
          </div>
        ) : regions.map(region => (
          <div key={region.id} style={{
            background: 'var(--surface-0)', border: '1px solid var(--line-1)', padding: 16,
          }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 10 }}>
              <div>
                <div style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--ink-0)' }}>{region.display_name}</div>
                <div style={{ fontSize: 8.5, color: 'var(--ink-4)', marginTop: 2 }}>{region.provider.toUpperCase()} · {region.name}</div>
              </div>
              <div style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
                <div style={{ width: 6, height: 6, borderRadius: '50%', background: statusColor(region.status) }} />
                <span style={{ fontSize: 8, color: statusColor(region.status), letterSpacing: '0.1em' }}>{region.status.toUpperCase()}</span>
              </div>
            </div>
            <div style={{ display: 'flex', gap: 16 }}>
              <div style={{ textAlign: 'center' }}>
                <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--cyan)' }}>{region.agent_count}</div>
                <div style={{ fontSize: 7.5, color: 'var(--ink-4)', letterSpacing: '0.1em' }}>AGENTS</div>
              </div>
              <div style={{ textAlign: 'center' }}>
                <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--violet)' }}>{region.cluster_count}</div>
                <div style={{ fontSize: 7.5, color: 'var(--ink-4)', letterSpacing: '0.1em' }}>CLUSTERS</div>
              </div>
              {region.is_primary && (
                <div style={{ marginLeft: 'auto', alignSelf: 'flex-end', fontSize: 7.5, color: 'var(--amber)', letterSpacing: '0.1em', background: 'var(--amber-dim)', padding: '2px 6px' }}>
                  PRIMARY
                </div>
              )}
            </div>
          </div>
        ))}
      </div>

      {/* Recent placements */}
      {placements.length > 0 && (
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>RECENT PLACEMENTS</span>
          </div>
          <div style={{ padding: '0 14px' }}>
            {placements.map((p, i) => (
              <div key={i} style={{ display: 'flex', gap: 12, padding: '7px 0', borderBottom: '1px solid var(--line-0)', fontSize: 9 }}>
                <span style={{ color: 'var(--ink-3)', fontFamily: 'monospace' }}>{p.agent_id?.slice(0, 8)}</span>
                <span style={{ color: 'var(--cyan)' }}>→</span>
                <span style={{ color: 'var(--ink-1)' }}>{p.region_name || p.region_id?.slice(0, 8)}</span>
                <span style={{ color: 'var(--ink-4)', marginLeft: 'auto' }}>{p.created_at?.slice(11, 19)}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}