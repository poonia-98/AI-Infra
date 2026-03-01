'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { useAuth } from '@/contexts/AuthContext';
import {
  LayoutDashboard, Bot, Activity, Bell,
  CreditCard, Eye, Shield, FlaskConical,
  Globe, GitBranch, Dna, BookOpen, Lock,
  LogOut, Clock, Server, MemoryStick,
  Brain, Cpu, Zap, TrendingUp, Radio,
} from 'lucide-react';

interface NavItem {
  href: string;
  label: string;
  icon: React.ComponentType<{ style?: React.CSSProperties }>;
  key: string;
  badge?: string;
}

interface NavSection {
  label: string;
  items: NavItem[];
}

const NAV: NavSection[] = [
  {
    label: 'CONTROL PLANE',
    items: [
      { href: '/brain',    label: 'Control Brain',  icon: Brain,       key: 'BRAN' },
      { href: '/services', label: 'Services',        icon: Server,      key: 'SVCS' },
      { href: '/memory',   label: 'Memory Engine',   icon: MemoryStick, key: 'MEMO' },
    ],
  },
  {
    label: 'CORE',
    items: [
      { href: '/',           label: 'Dashboard',    icon: LayoutDashboard, key: 'DASH' },
      { href: '/agents',     label: 'Agents',       icon: Bot,             key: 'AGNT' },
      { href: '/monitoring', label: 'Monitoring',   icon: Activity,        key: 'MNTR' },
      { href: '/monitoring', label: 'Alerts',       icon: Bell,            key: 'ALRT' },
    ],
  },
  {
    label: 'CLOUD PLATFORM',
    items: [
      { href: '/billing',       label: 'Billing',       icon: CreditCard,   key: 'BILL' },
      { href: '/observability', label: 'Observability', icon: Eye,          key: 'OBSV' },
      { href: '/sandbox',       label: 'Sandbox',       icon: Shield,       key: 'SBOX' },
      { href: '/simulation',    label: 'Simulation',    icon: FlaskConical, key: 'SIML' },
    ],
  },
  {
    label: 'INFRASTRUCTURE',
    items: [
      { href: '/regions',    label: 'Regions',    icon: Globe,     key: 'RGIO' },
      { href: '/federation', label: 'Federation', icon: GitBranch, key: 'FDTN' },
    ],
  },
  {
    label: 'AI CAPABILITIES',
    items: [
      { href: '/evolution', label: 'Evolution',  icon: Dna,      key: 'EVOL' },
      { href: '/knowledge', label: 'Knowledge',  icon: BookOpen, key: 'KNOW' },
      { href: '/vault',     label: 'Vault',      icon: Lock,     key: 'VALT' },
    ],
  },
  {
    label: 'DEVELOPER TOOLS',
    items: [
      { href: '/time-travel', label: 'Time Travel', icon: Clock, key: 'TRVL' },
    ],
  },
];

const ICON_SIZE = { width: 11, height: 11 };
const SECTION_ICON_SIZE = { width: 8, height: 8 };

export default function Sidebar() {
  const pathname = usePathname();
  const { user, logout } = useAuth();

  const isActive = (href: string) =>
    href === '/' ? pathname === '/' : pathname.startsWith(href);

  return (
    <nav style={{
      width: 200,
      height: '100vh',
      background: 'var(--surface-0)',
      borderRight: '1px solid var(--line-1)',
      display: 'flex',
      flexDirection: 'column',
      flexShrink: 0,
      fontFamily: 'var(--mono)',
    }}>

      {/* Logo */}
      <div style={{
        padding: '16px 14px 12px',
        borderBottom: '1px solid var(--line-1)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <div style={{
            width: 26, height: 26,
            background: 'linear-gradient(135deg, var(--cyan), var(--violet))',
            clipPath: 'polygon(50% 0%, 100% 25%, 100% 75%, 50% 100%, 0% 75%, 0% 25%)',
            flexShrink: 0,
            position: 'relative',
          }}>
            <div style={{
              position: 'absolute', inset: 3,
              background: 'var(--surface-0)',
              clipPath: 'polygon(50% 0%, 100% 25%, 100% 75%, 50% 100%, 0% 75%, 0% 25%)',
            }} />
          </div>
          <div>
            <div style={{
              fontSize: 11, fontWeight: 700, letterSpacing: '0.12em',
              color: 'var(--ink-0)', fontFamily: 'var(--display)',
            }}>
              AGENTPLANE
            </div>
            <div style={{ fontSize: 7.5, color: 'var(--cyan)', letterSpacing: '0.18em' }}>
              v3.0 · AI OS
            </div>
          </div>
        </div>
      </div>

      {/* Nav */}
      <div style={{ flex: 1, overflowY: 'auto', padding: '8px 0' }}>
        {NAV.map((section) => (
          <div key={section.label} style={{ marginBottom: 4 }}>
            <div style={{
              padding: '6px 14px 3px',
              fontSize: 7.5, letterSpacing: '0.22em',
              color: 'var(--ink-4)', fontWeight: 700,
            }}>
              {section.label}
            </div>
            {section.items.map((item) => {
              const active = isActive(item.href);
              const Icon = item.icon;
              return (
                <Link
                  key={item.key}
                  href={item.href}
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    gap: 8,
                    padding: '5px 14px',
                    textDecoration: 'none',
                    background: active ? 'rgba(0,217,255,0.08)' : 'transparent',
                    borderLeft: active
                      ? '2px solid var(--cyan)'
                      : '2px solid transparent',
                    transition: 'all 0.1s',
                    cursor: 'pointer',
                    position: 'relative',
                  }}
                  onMouseEnter={(e) => {
                    if (!active) {
                      (e.currentTarget as HTMLAnchorElement).style.background = 'rgba(255,255,255,0.03)';
                    }
                  }}
                  onMouseLeave={(e) => {
                    if (!active) {
                      (e.currentTarget as HTMLAnchorElement).style.background = 'transparent';
                    }
                  }}
                >
                  {/* Key badge */}
                  <span style={{
                    fontSize: 6, letterSpacing: '0.08em',
                    color: active ? 'var(--cyan)' : 'var(--ink-4)',
                    width: 22, flexShrink: 0,
                    fontWeight: active ? 700 : 400,
                  }}>
                    {item.key}
                  </span>

                  <Icon style={{
                    ...ICON_SIZE,
                    color: active ? 'var(--cyan)' : 'var(--ink-3)',
                    flexShrink: 0,
                  }} />

                  <span style={{
                    fontSize: 10,
                    color: active ? 'var(--ink-0)' : 'var(--ink-2)',
                    fontWeight: active ? 600 : 400,
                    letterSpacing: '0.04em',
                    flex: 1,
                  }}>
                    {item.label}
                  </span>

                  {item.badge && (
                    <span style={{
                      fontSize: 7, padding: '1px 4px',
                      background: 'var(--cyan)',
                      color: 'var(--surface-0)',
                      borderRadius: 2,
                    }}>
                      {item.badge}
                    </span>
                  )}
                </Link>
              );
            })}
          </div>
        ))}
      </div>

      {/* Stream status */}
      <div style={{
        padding: '6px 14px',
        borderTop: '1px solid var(--line-0)',
        display: 'flex', alignItems: 'center', gap: 5,
      }}>
        <div style={{
          width: 5, height: 5, borderRadius: '50%',
          background: 'var(--lime)',
          boxShadow: '0 0 4px var(--lime)',
          animation: 'pulse 2s ease-in-out infinite',
        }} />
        <span style={{ fontSize: 7.5, color: 'var(--ink-4)', letterSpacing: '0.12em' }}>
          STREAM LIVE
        </span>
        <Radio style={{ width: 8, height: 8, color: 'var(--lime)', marginLeft: 'auto' }} />
      </div>

      {/* User profile */}
      <div style={{
        padding: '8px 14px',
        borderTop: '1px solid var(--line-1)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
          <div style={{
            width: 22, height: 22, borderRadius: '50%',
            background: 'linear-gradient(135deg, var(--violet), var(--cyan))',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            fontSize: 8, color: 'white', fontWeight: 700, flexShrink: 0,
          }}>
            {(user?.name || user?.email || 'A').charAt(0).toUpperCase()}
          </div>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{
              fontSize: 9, color: 'var(--ink-1)',
              overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              fontWeight: 600,
            }}>
              {user?.name || user?.email?.split('@')[0] || 'Admin'}
            </div>
            <div style={{ fontSize: 7.5, color: 'var(--ink-4)', letterSpacing: '0.1em' }}>
              {(user as any)?.role?.toUpperCase() || 'ADMIN'}
            </div>
          </div>
        </div>
        <button
          onClick={logout}
          style={{
            width: '100%', padding: '4px 8px',
            display: 'flex', alignItems: 'center', gap: 6,
            background: 'transparent',
            border: '1px solid var(--line-1)',
            color: 'var(--ink-3)', cursor: 'pointer',
            fontSize: 8.5, letterSpacing: '0.1em',
            transition: 'all 0.15s',
            fontFamily: 'var(--mono)',
          }}
          onMouseEnter={(e) => {
            (e.currentTarget as HTMLButtonElement).style.borderColor = 'var(--crimson)';
            (e.currentTarget as HTMLButtonElement).style.color = 'var(--crimson)';
          }}
          onMouseLeave={(e) => {
            (e.currentTarget as HTMLButtonElement).style.borderColor = 'var(--line-1)';
            (e.currentTarget as HTMLButtonElement).style.color = 'var(--ink-3)';
          }}
        >
          <LogOut style={{ width: 9, height: 9 }} />
          SIGN OUT
        </button>
      </div>
    </nav>
  );
}