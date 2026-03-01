'use client';
import { useEffect, useState } from 'react';
import { api, BillingPlan, Subscription } from '@/lib/api';
import { CreditCard, TrendingUp, FileText, Zap } from 'lucide-react';

function PanelHeader({ title, sub }: { title: string; sub?: string }) {
  return (
    <div style={{ padding: '8px 16px', borderBottom: '1px solid var(--line-1)', display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
      <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>{title}</span>
      {sub && <span style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{sub}</span>}
    </div>
  );
}

export default function BillingPage() {
  const [plans, setPlans] = useState<BillingPlan[]>([]);
  const [usage, setUsage] = useState<any[]>([]);
  const [invoices, setInvoices] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api.billing.plans().catch(() => []),
      api.billing.usage('', '30d').catch(() => []),
      api.billing.invoices('', 10).catch(() => []),
    ]).then(([p, u, i]) => {
      setPlans(p); setUsage(u); setInvoices(i); setLoading(false);
    });
  }, []);

  if (loading) return (
    <div style={{ padding: 32, color: 'var(--ink-4)', fontSize: 10, letterSpacing: '0.1em' }}>
      LOADING BILLING DATA...
    </div>
  );

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>
          BILLING
        </div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>
          PLANS · SUBSCRIPTIONS · INVOICES · USAGE
        </div>
      </div>

      {/* Plans grid */}
      <div style={{ marginBottom: 24 }}>
        <div style={{ fontSize: 9, letterSpacing: '0.18em', color: 'var(--ink-4)', fontWeight: 700, marginBottom: 10 }}>
          AVAILABLE PLANS
        </div>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))', gap: 12 }}>
          {plans.length === 0 ? (
            ['STARTER', 'GROWTH', 'SCALE', 'ENTERPRISE'].map(name => (
              <div key={name} style={{ background: 'var(--surface-1)', border: '1px solid var(--line-1)', padding: 16 }}>
                <div style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em', marginBottom: 4 }}>{name}</div>
                <div style={{ fontSize: 9, color: 'var(--ink-4)' }}>Connect billing service</div>
              </div>
            ))
          ) : plans.map(plan => (
            <div key={plan.id} style={{
              background: 'var(--surface-1)', border: '1px solid var(--line-1)', padding: 16,
              transition: 'border-color 0.15s',
            }}
              onMouseEnter={e => (e.currentTarget as HTMLDivElement).style.borderColor = 'var(--cyan)'}
              onMouseLeave={e => (e.currentTarget as HTMLDivElement).style.borderColor = 'var(--line-1)'}
            >
              <div style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--cyan)', letterSpacing: '0.08em' }}>
                {plan.display_name}
              </div>
              <div style={{ fontSize: 18, fontWeight: 700, color: 'var(--ink-0)', margin: '8px 0 4px' }}>
                ${plan.price_monthly_usd}<span style={{ fontSize: 9, color: 'var(--ink-4)' }}>/mo</span>
              </div>
              <div style={{ fontSize: 8.5, color: 'var(--ink-4)', lineHeight: 1.8 }}>
                <div>{plan.max_agents} agents</div>
                <div>{plan.max_executions_day.toLocaleString()} exec/day</div>
                <div>{plan.max_api_rpm} API RPM</div>
              </div>
            </div>
          ))}
        </div>
      </div>

      {/* Usage + Invoices */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <PanelHeader title="CURRENT USAGE" sub="LAST 30 DAYS" />
          <div style={{ padding: 16 }}>
            {usage.length === 0 ? (
              <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '24px 0' }}>
                No usage data available
              </div>
            ) : usage.map((u, i) => (
              <div key={i} style={{ display: 'flex', justifyContent: 'space-between', padding: '5px 0', borderBottom: '1px solid var(--line-0)' }}>
                <span style={{ fontSize: 9.5, color: 'var(--ink-2)', letterSpacing: '0.06em' }}>{u.resource_type}</span>
                <span style={{ fontSize: 9.5, color: 'var(--cyan)' }}>{u.quantity} {u.unit}</span>
              </div>
            ))}
          </div>
        </div>

        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <PanelHeader title="INVOICES" sub="RECENT" />
          <div style={{ padding: 16 }}>
            {invoices.length === 0 ? (
              <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '24px 0' }}>
                No invoices available
              </div>
            ) : invoices.map((inv, i) => (
              <div key={i} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '6px 0', borderBottom: '1px solid var(--line-0)' }}>
                <div>
                  <div style={{ fontSize: 9.5, color: 'var(--ink-1)' }}>{inv.period_label || inv.id?.slice(0, 8)}</div>
                  <div style={{ fontSize: 8.5, color: 'var(--ink-4)' }}>{inv.created_at?.slice(0, 10)}</div>
                </div>
                <div style={{ textAlign: 'right' }}>
                  <div style={{ fontSize: 11, color: 'var(--ink-0)', fontWeight: 600 }}>${inv.amount_usd ?? '—'}</div>
                  <div style={{ fontSize: 8, color: inv.status === 'paid' ? 'var(--lime)' : 'var(--amber)', letterSpacing: '0.08em' }}>
                    {(inv.status || 'pending').toUpperCase()}
                  </div>
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}