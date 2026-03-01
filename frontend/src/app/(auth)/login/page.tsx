'use client';

import { useState, useEffect, useRef } from 'react';
import { useAuth } from '@/contexts/AuthContext';
import { useRouter, useSearchParams } from 'next/navigation';

const BOOT_LINES = [
  'AGENTPLANE CONTROL PLANE v3.0',
  'Initializing secure enclave...',
  'Loading cryptographic modules... OK',
  'Verifying system integrity... OK',
  'Establishing encrypted channel... OK',
  'All systems nominal.',
  '─────────────────────────────────────',
  'AUTHORIZED PERSONNEL ONLY.',
  'All access attempts are logged and monitored.',
];

export default function LoginPage() {
  const { login, isAuthenticated, loading } = useAuth();
  const router = useRouter();
  const searchParams = useSearchParams();
  const redirect = searchParams.get('redirect') || '/';

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [bootLines, setBootLines] = useState<string[]>([]);
  const [bootDone, setBootDone] = useState(false);
  const [attempts, setAttempts] = useState(0);
  const [locked, setLocked] = useState(false);
  const [lockCountdown, setLockCountdown] = useState(0);
  const emailRef = useRef<HTMLInputElement>(null);

  // Boot sequence animation
  useEffect(() => {
  let i = 0;

  const interval = setInterval(() => {
    if (i >= BOOT_LINES.length) {
      clearInterval(interval);
      setTimeout(() => setBootDone(true), 400);
      return;
    }

    const line = BOOT_LINES[i];

    if (line) {
      setBootLines(prev => [...prev, line]);
    }

    i++;
  }, 120);

  return () => {
    clearInterval(interval);
  };
}, []);

  // Focus email when boot done
  useEffect(() => {
    if (bootDone) setTimeout(() => emailRef.current?.focus(), 100);
  }, [bootDone]);

  // Lock countdown
  useEffect(() => {
    if (!locked) return;
    const interval = setInterval(() => {
      setLockCountdown(prev => {
        if (prev <= 1) { setLocked(false); setAttempts(0); clearInterval(interval); return 0; }
        return prev - 1;
      });
    }, 1000);
    return () => clearInterval(interval);
  }, [locked]);

  // Redirect if already authed
  useEffect(() => {
    if (!loading && isAuthenticated) router.replace(redirect);
  }, [isAuthenticated, loading, router, redirect]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (locked || submitting) return;
    if (!email.trim() || !password) { setError('All fields required.'); return; }

    setSubmitting(true);
    setError('');

    const result = await login(email.trim(), password);

    if (result.success) {
      router.replace(redirect);
    } else {
      const newAttempts = attempts + 1;
      setAttempts(newAttempts);
      setError(result.error || 'Authentication failed');
      setPassword('');

      if (newAttempts >= 5) {
        setLocked(true);
        setLockCountdown(30);
        setError('Too many failed attempts. Access locked for 30s.');
      }
    }
    setSubmitting(false);
  }

  return (
    <div style={{
      position: 'fixed', inset: 0,
      background: 'var(--void)',
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      overflow: 'hidden',
    }}>
      {/* Animated background grid */}
      <div style={{
        position: 'absolute', inset: 0,
        backgroundImage: 'linear-gradient(rgba(0,217,255,0.025) 1px, transparent 1px), linear-gradient(90deg, rgba(0,217,255,0.025) 1px, transparent 1px)',
        backgroundSize: '48px 48px',
      }} />

      {/* Scan line effect */}
      <div style={{
        position: 'absolute', inset: 0, pointerEvents: 'none',
        background: 'repeating-linear-gradient(0deg, transparent, transparent 2px, rgba(0,0,0,0.06) 2px, rgba(0,0,0,0.06) 4px)',
      }} />

      {/* Glow orbs */}
      <div style={{
        position: 'absolute', top: '20%', left: '15%', width: 400, height: 400,
        background: 'radial-gradient(circle, rgba(0,217,255,0.04) 0%, transparent 70%)',
        pointerEvents: 'none',
      }} />
      <div style={{
        position: 'absolute', bottom: '20%', right: '15%', width: 500, height: 500,
        background: 'radial-gradient(circle, rgba(168,85,247,0.04) 0%, transparent 70%)',
        pointerEvents: 'none',
      }} />

      {/* Corner markers */}
      {[
        { top: 20, left: 20, borderTop: '1px solid', borderLeft: '1px solid' },
        { top: 20, right: 20, borderTop: '1px solid', borderRight: '1px solid' },
        { bottom: 20, left: 20, borderBottom: '1px solid', borderLeft: '1px solid' },
        { bottom: 20, right: 20, borderBottom: '1px solid', borderRight: '1px solid' },
      ].map((style, i) => (
        <div key={i} style={{
          position: 'absolute', width: 32, height: 32,
          borderColor: 'rgba(0,217,255,0.2)',
          ...style,
        }} />
      ))}

      {/* Top system status bar */}
      <div style={{
        position: 'absolute', top: 20, left: '50%', transform: 'translateX(-50%)',
        display: 'flex', alignItems: 'center', gap: 20,
        fontSize: 9, letterSpacing: '0.15em', color: 'var(--ink-4)',
      }}>
        <span>SYS:NOMINAL</span>
        <span style={{ color: 'var(--line-2)' }}>|</span>
        <span style={{ color: 'var(--lime)', fontSize: 8 }}>● SECURE CHANNEL</span>
        <span style={{ color: 'var(--line-2)' }}>|</span>
        <span>{new Date().toISOString().slice(0, 19).replace('T', ' ')} UTC</span>
      </div>

      {/* Main container */}
      <div style={{
        position: 'relative',
        width: '100%',
        maxWidth: 500,
        margin: '0 24px',
        animation: 'fade-in 0.4s ease',
      }}>
        {/* Logo block */}
        <div style={{ textAlign: 'center', marginBottom: 40 }}>
          <div style={{ display: 'inline-flex', alignItems: 'center', gap: 14, marginBottom: 10 }}>
            <svg viewBox="0 0 40 40" width="40" height="40">
              <polygon points="20,2 38,11 38,29 20,38 2,29 2,11"
                fill="none" stroke="var(--cyan)" strokeWidth="1" opacity="0.6" />
              <polygon points="20,8 32,15 32,25 20,32 8,25 8,15"
                fill="rgba(0,217,255,0.08)" stroke="var(--cyan)" strokeWidth="0.5" />
              <polygon points="20,14 26,18 26,22 20,26 14,22 14,18"
                fill="rgba(0,217,255,0.15)" stroke="var(--cyan)" strokeWidth="0.5" />
              <circle cx="20" cy="20" r="3" fill="var(--cyan)" />
            </svg>
            <div>
              <div style={{
                fontFamily: 'var(--display)', fontSize: 28, fontWeight: 700,
                color: 'var(--ink-0)', letterSpacing: '0.12em',
              }}>AGENTPLANE</div>
              <div style={{
                fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.2em', marginTop: 2,
              }}>CONTROL PLANE — RESTRICTED ACCESS</div>
            </div>
          </div>
        </div>

        {/* Boot terminal */}
        {!bootDone && (
          <div style={{
            background: 'rgba(0,0,0,0.5)',
            border: '1px solid var(--line-1)',
            padding: '16px 20px',
            marginBottom: 24,
            minHeight: 140,
          }}>
            {bootLines.map((line, i) => (
              <div key={i} style={{
                fontSize: 10, fontFamily: 'var(--mono)',
                color: line.includes('OK') ? 'var(--lime)'
                  : line.includes('AUTHORIZED') ? 'var(--crimson)'
                  : line.includes('AGENTPLANE') ? 'var(--cyan)'
                  : line.startsWith('─') ? 'var(--line-2)'
                  : 'var(--ink-2)',
                lineHeight: 1.8, letterSpacing: '0.04em',
                animation: 'fade-in 0.15s ease',
              }}>
                {line.startsWith('─') ? line : `> ${line}`}
              </div>
            ))}
            {!bootDone && (
              <span style={{ fontSize: 10, color: 'var(--cyan)' }} className="anim-cursor">_</span>
            )}
          </div>
        )}

        {/* Auth form */}
        {bootDone && (
          <div style={{ animation: 'slide-in-top 0.25s cubic-bezier(0.16,1,0.3,1)' }}>
            {/* Panel header */}
            <div style={{
              display: 'flex', alignItems: 'center', justifyContent: 'space-between',
              padding: '8px 14px',
              background: 'var(--surface-0)',
              border: '1px solid var(--line-1)',
              borderBottom: '1px solid var(--cyan)',
            }}>
              <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>
                IDENTITY VERIFICATION
              </span>
              <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                <div style={{ width: 6, height: 6, borderRadius: '50%', background: 'var(--lime)', animation: 'breathe-green 2s ease-in-out infinite' }} />
                <span style={{ fontSize: 8, color: 'var(--lime)', letterSpacing: '0.1em' }}>ENCRYPTED</span>
              </div>
            </div>

            <div style={{
              background: 'var(--surface-0)',
              border: '1px solid var(--line-1)',
              borderTop: 'none',
              padding: '28px 28px 24px',
              animation: 'border-glow-pulse 4s ease-in-out infinite',
            }}>
              <form onSubmit={handleSubmit}>
                {/* Email */}
                <div style={{ marginBottom: 16 }}>
                  <label style={{
                    display: 'block', fontSize: 8.5, letterSpacing: '0.18em',
                    color: 'var(--ink-3)', fontWeight: 700, marginBottom: 6,
                  }}>
                    OPERATOR EMAIL
                  </label>
                  <input
                    ref={emailRef}
                    type="email"
                    value={email}
                    onChange={e => setEmail(e.target.value)}
                    disabled={submitting || locked}
                    autoComplete="username"
                    spellCheck={false}
                    style={{
                      width: '100%', padding: '10px 12px',
                      background: 'var(--surface-1)',
                      border: `1px solid ${error && !email ? 'var(--crimson)' : 'var(--line-2)'}`,
                      color: 'var(--ink-0)',
                      fontFamily: 'var(--mono)', fontSize: 12,
                      outline: 'none', letterSpacing: '0.03em',
                      transition: 'border-color 0.15s',
                    }}
                    onFocus={e => e.target.style.borderColor = 'var(--cyan)'}
                    onBlur={e => e.target.style.borderColor = 'var(--line-2)'}
                    placeholder="operator@domain.com"
                  />
                </div>

                {/* Password */}
                <div style={{ marginBottom: 20 }}>
                  <label style={{
                    display: 'block', fontSize: 8.5, letterSpacing: '0.18em',
                    color: 'var(--ink-3)', fontWeight: 700, marginBottom: 6,
                  }}>
                    ACCESS CREDENTIAL
                  </label>
                  <input
                    type="password"
                    value={password}
                    onChange={e => setPassword(e.target.value)}
                    disabled={submitting || locked}
                    autoComplete="current-password"
                    style={{
                      width: '100%', padding: '10px 12px',
                      background: 'var(--surface-1)',
                      border: `1px solid ${error && !password ? 'var(--crimson)' : 'var(--line-2)'}`,
                      color: 'var(--ink-0)',
                      fontFamily: 'var(--mono)', fontSize: 12,
                      outline: 'none', letterSpacing: '0.06em',
                      transition: 'border-color 0.15s',
                    }}
                    onFocus={e => e.target.style.borderColor = 'var(--cyan)'}
                    onBlur={e => e.target.style.borderColor = 'var(--line-2)'}
                    placeholder="••••••••••••"
                  />
                </div>

                {/* Error message */}
                {error && (
                  <div style={{
                    padding: '8px 12px', marginBottom: 16,
                    background: 'var(--crimson-dim)',
                    border: '1px solid rgba(255,61,85,0.3)',
                    fontSize: 9.5, color: 'var(--crimson)',
                    letterSpacing: '0.06em',
                    display: 'flex', alignItems: 'center', gap: 8,
                  }}>
                    <span>⚠</span>
                    {locked ? `ACCESS LOCKED — ${lockCountdown}s remaining` : error}
                  </div>
                )}

                {/* Attempt indicator */}
                {attempts > 0 && !locked && (
                  <div style={{
                    display: 'flex', gap: 4, marginBottom: 14, alignItems: 'center',
                  }}>
                    <span style={{ fontSize: 8, color: 'var(--ink-4)', letterSpacing: '0.1em', marginRight: 4 }}>
                      ATTEMPTS:
                    </span>
                    {Array.from({ length: 5 }).map((_, i) => (
                      <div key={i} style={{
                        width: 8, height: 8, borderRadius: '50%',
                        background: i < attempts ? 'var(--crimson)' : 'var(--line-2)',
                        transition: 'background 0.2s',
                      }} />
                    ))}
                    <span style={{ fontSize: 8, color: 'var(--crimson)', marginLeft: 4 }}>
                      {5 - attempts} remaining
                    </span>
                  </div>
                )}

                {/* Submit button */}
                <button
                  type="submit"
                  disabled={submitting || locked}
                  style={{
                    width: '100%', padding: '11px',
                    background: locked ? 'rgba(255,61,85,0.08)' : submitting ? 'rgba(0,217,255,0.06)' : 'rgba(0,217,255,0.10)',
                    border: `1px solid ${locked ? 'var(--crimson)' : 'var(--cyan)'}`,
                    color: locked ? 'var(--crimson)' : 'var(--cyan)',
                    fontFamily: 'var(--display)', fontSize: 13, fontWeight: 700,
                    letterSpacing: '0.18em', cursor: locked || submitting ? 'not-allowed' : 'pointer',
                    transition: 'all 0.15s',
                    opacity: locked ? 0.6 : 1,
                  }}
                  onMouseEnter={e => {
                    if (!locked && !submitting) {
                      (e.target as HTMLButtonElement).style.background = 'rgba(0,217,255,0.18)';
                      (e.target as HTMLButtonElement).style.boxShadow = '0 0 24px rgba(0,217,255,0.2)';
                    }
                  }}
                  onMouseLeave={e => {
                    (e.target as HTMLButtonElement).style.background = 'rgba(0,217,255,0.10)';
                    (e.target as HTMLButtonElement).style.boxShadow = 'none';
                  }}
                >
                  {locked ? `⊘ ACCESS LOCKED (${lockCountdown}s)`
                    : submitting ? '◌ AUTHENTICATING...'
                    : '→ AUTHENTICATE'}
                </button>
              </form>
            </div>

            {/* Security notice */}
            <div style={{
              marginTop: 16, padding: '10px 14px',
              border: '1px solid var(--line-0)',
              background: 'rgba(0,0,0,0.3)',
              display: 'flex', alignItems: 'flex-start', gap: 10,
            }}>
              <span style={{ fontSize: 10, color: 'var(--crimson)', flexShrink: 0 }}>⚠</span>
              <p style={{
                fontSize: 8.5, color: 'var(--ink-4)', lineHeight: 1.7,
                letterSpacing: '0.04em',
              }}>
                This system is for authorized operators only. Unauthorized access attempts are
                logged, traced, and reported. All sessions are monitored and audited.
              </p>
            </div>
          </div>
        )}
      </div>

      {/* Bottom status */}
      <div style={{
        position: 'absolute', bottom: 20, left: '50%', transform: 'translateX(-50%)',
        display: 'flex', gap: 24, fontSize: 8.5, letterSpacing: '0.12em', color: 'var(--ink-4)',
      }}>
        <span>TLS 1.3</span>
        <span style={{ color: 'var(--line-2)' }}>|</span>
        <span>AES-256-GCM</span>
        <span style={{ color: 'var(--line-2)' }}>|</span>
        <span>SESSIONS AUDITED</span>
      </div>
    </div>
  );
}