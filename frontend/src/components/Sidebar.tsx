'use client';
import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { LayoutDashboard, Bot, Activity } from 'lucide-react';
import { useWebSocket } from '@/hooks/useWebSocket';
import clsx from 'clsx';

const NAV = [
  { href: '/',           label: 'Dashboard',  icon: LayoutDashboard, key: 'CTRL' },
  { href: '/agents',     label: 'Agents',     icon: Bot,             key: 'AGNT' },
  { href: '/monitoring', label: 'Monitoring', icon: Activity,        key: 'MNTR' },
];

export default function Sidebar() {
  const path = usePathname();
  const { connected, throughput } = useWebSocket();

  const evPerSec  = (throughput['agents.events']  ?? 0).toFixed(1);
  const metPerSec = (throughput['metrics.stream'] ?? 0).toFixed(1);
  const logPerSec = (throughput['logs.stream']    ?? 0).toFixed(1);
  const totalRate = ((throughput['agents.events'] ?? 0) + (throughput['metrics.stream'] ?? 0) + (throughput['logs.stream'] ?? 0)).toFixed(1);

  return (
    <aside
      className="flex-shrink-0 flex flex-col"
      style={{
        width: 196,
        background: 'var(--surface-0)',
        borderRight: '1px solid var(--line-1)',
      }}
    >
      {/* ── Logo ─────────────────────────────────────────── */}
      <div style={{ padding: '14px 12px 12px', borderBottom: '1px solid var(--line-1)' }}>
        <div className="flex items-center gap-2.5 mb-3">
          {/* Hexagonal icon */}
          <div style={{ position: 'relative', width: 28, height: 28 }}>
            <svg viewBox="0 0 28 28" width="28" height="28">
              <polygon
                points="14,2 26,8 26,20 14,26 2,20 2,8"
                fill="none"
                stroke="var(--cyan)"
                strokeWidth="1"
                opacity="0.7"
              />
              <polygon
                points="14,7 21,11 21,17 14,21 7,17 7,11"
                fill="rgba(0,217,255,0.15)"
                stroke="var(--cyan)"
                strokeWidth="0.5"
              />
              <circle cx="14" cy="14" r="3" fill="var(--cyan)" />
            </svg>
            {/* Pulsing ring */}
            <div
              style={{
                position: 'absolute', inset: 2, borderRadius: '50%',
                border: '1px solid var(--cyan)', opacity: 0,
                animation: 'ring-expand 2.5s ease-out infinite',
              }}
            />
          </div>
          <div>
            <div style={{ fontFamily: 'var(--display)', fontSize: 15, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.05em' }}>
              AGENTPLANE
            </div>
            <div style={{ fontSize: 8.5, color: 'var(--ink-4)', letterSpacing: '0.1em', marginTop: 1 }}>
              CONTROL PLANE v1.0
            </div>
          </div>
        </div>

        {/* Stream status */}
        <div
          style={{
            padding: '5px 8px',
            background: connected ? 'rgba(57,255,20,0.06)' : 'rgba(255,61,85,0.07)',
            border: `1px solid ${connected ? 'rgba(57,255,20,0.2)' : 'rgba(255,61,85,0.2)'}`,
            display: 'flex', alignItems: 'center', gap: 7,
          }}
        >
          <div className="beacon" style={{ width: 12, height: 12 }}>
            <div className={`beacon-core ${connected ? 'beacon-online' : 'beacon-error'}`}
              style={{ width: 6, height: 6 }} />
            {connected && (
              <div className="beacon-ring beacon-online"
                style={{ position: 'absolute', inset: 0, borderRadius: '50%' }} />
            )}
          </div>
          <span style={{ fontSize: 9.5, fontWeight: 700, letterSpacing: '0.1em', color: connected ? 'var(--lime)' : 'var(--crimson)' }}>
            {connected ? 'STREAM ACTIVE' : 'DISCONNECTED'}
          </span>
          {connected && (
            <span style={{ fontSize: 9, color: 'var(--ink-4)', marginLeft: 'auto' }}>
              {totalRate}/s
            </span>
          )}
        </div>
      </div>

      {/* ── Nav ──────────────────────────────────────────── */}
      <nav style={{ flex: 1, padding: '10px 0', overflowY: 'auto' }}>
        <div style={{ padding: '0 12px 6px', fontSize: 8.5, letterSpacing: '0.15em', color: 'var(--ink-4)', fontWeight: 700 }}>
          NAVIGATION
        </div>
        {NAV.map(({ href, label, icon: Icon, key }) => {
          const active = path === href;
          return (
            <Link key={href} href={href} className={clsx('nav-pill', active && 'active')}>
              <div
                style={{
                  width: 22, height: 22, display: 'flex', alignItems: 'center', justifyContent: 'center',
                  background: active ? 'rgba(0,217,255,0.12)' : 'transparent',
                  border: `1px solid ${active ? 'rgba(0,217,255,0.3)' : 'transparent'}`,
                  flexShrink: 0,
                }}
              >
                <Icon style={{ width: 12, height: 12, color: active ? 'var(--cyan)' : 'var(--ink-3)' }} />
              </div>
              <span style={{ flex: 1, letterSpacing: '0.04em' }}>{label}</span>
              <span style={{ fontSize: 8, color: active ? 'var(--cyan)' : 'var(--ink-4)', letterSpacing: '0.08em', opacity: 0.6 }}>
                {key}
              </span>
            </Link>
          );
        })}
      </nav>

      {/* ── Telemetry footer ─────────────────────────────── */}
      <div style={{ borderTop: '1px solid var(--line-1)', padding: '10px 12px' }}>
        <div style={{ fontSize: 8.5, letterSpacing: '0.15em', color: 'var(--ink-4)', fontWeight: 700, marginBottom: 6 }}>
          STREAM TELEMETRY
        </div>
        {[
          { label: 'EVENTS', value: evPerSec,  color: connected && parseFloat(evPerSec) > 0 ? 'var(--cyan)' : 'var(--ink-4)' },
          { label: 'METRICS', value: metPerSec, color: connected && parseFloat(metPerSec) > 0 ? 'var(--lime)' : 'var(--ink-4)' },
          { label: 'LOGS',   value: logPerSec,  color: connected && parseFloat(logPerSec) > 0 ? 'var(--violet)' : 'var(--ink-4)' },
        ].map(({ label, value, color }) => (
          <div key={label} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '2px 0' }}>
            <span style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.08em' }}>{label}</span>
            <span style={{ fontSize: 9, color, fontWeight: 600 }}>{value}/s</span>
          </div>
        ))}

        {/* Tiny bandwidth bar */}
        <div style={{ marginTop: 6, height: 1, background: 'var(--line-1)', position: 'relative', overflow: 'hidden' }}>
          <div style={{
            position: 'absolute', left: 0, top: 0, bottom: 0,
            width: connected ? `${Math.min(100, parseFloat(totalRate) * 10)}%` : '0%',
            background: 'linear-gradient(90deg, var(--cyan), var(--lime))',
            transition: 'width 0.8s ease',
          }} />
        </div>
      </div>
    </aside>
  );
}