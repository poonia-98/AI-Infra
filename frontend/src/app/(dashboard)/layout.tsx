'use client';

import { useAuth } from '@/contexts/AuthContext';
import { useRouter } from 'next/navigation';
import { useEffect } from 'react';
import Sidebar from '@/components/Sidebar';

export default function DashboardLayout({ children }: { children: React.ReactNode }) {
  const { isAuthenticated, loading } = useAuth();
  const router = useRouter();

  useEffect(() => {
    if (!loading && !isAuthenticated) {
      router.replace('/login');
    }
  }, [isAuthenticated, loading, router]);

  if (loading) {
    return (
      <div style={{
        height: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center',
        background: 'var(--void)', flexDirection: 'column', gap: 12,
      }}>
        <div style={{ width: 24, height: 24 }}>
          <svg viewBox="0 0 40 40" width="24" height="24">
            <polygon points="20,2 38,11 38,29 20,38 2,29 2,11"
              fill="none" stroke="var(--cyan)" strokeWidth="1.5" opacity="0.6" />
            <circle cx="20" cy="20" r="3" fill="var(--cyan)" />
          </svg>
        </div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.2em' }}>
          VERIFYING SESSION...
        </div>
      </div>
    );
  }

  if (!isAuthenticated) return null;

  return (
    <div className="flex h-screen overflow-hidden grid-substrate">
      <Sidebar />
      <main className="flex-1 overflow-auto">{children}</main>
    </div>
  );
}