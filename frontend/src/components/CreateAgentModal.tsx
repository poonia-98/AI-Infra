'use client';
import { useState } from 'react';
import { X, Bot, Cpu, HardDrive, Code, Terminal, ChevronDown } from 'lucide-react';
import { api } from '@/lib/api';

interface Props { onClose: () => void; onCreated: () => void; }

const PRESETS = [
  { label: 'Data Analyzer', task: 'Analyze incoming data pipeline and generate quality report', iter: 5 },
  { label: 'Report Generator', task: 'Generate comprehensive system performance report', iter: 3 },
  { label: 'Stress Test', task: 'Run extended performance benchmarking suite', iter: 10 },
  { label: 'Quick Scan', task: 'Perform rapid system health check', iter: 2 },
];

export default function CreateAgentModal({ onClose, onCreated }: Props) {
  const [form, setForm] = useState({
    name: '', description: '', agent_type: 'langgraph',
    image: 'agent-runtime:latest', cpu_limit: '1.0',
    memory_limit: '512m', task: '', max_iterations: '5',
  });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [showAdvanced, setShowAdvanced] = useState(false);

  function applyPreset(p: typeof PRESETS[0]) {
    setForm(f => ({
      ...f,
      name: f.name || p.label.toLowerCase().replace(/ /g, '-'),
      task: p.task,
      max_iterations: String(p.iter),
    }));
  }

  async function handleSubmit() {
    if (!form.name.trim()) { setError('Agent name is required'); return; }
    setLoading(true); setError('');
    try {
      await api.agents.create({
        name: form.name.trim(),
        description: form.description || undefined,
        agent_type: form.agent_type,
        image: form.image,
        cpu_limit: parseFloat(form.cpu_limit) || 1.0,
        memory_limit: form.memory_limit,
        env_vars: form.task ? {
          TASK_INPUT: JSON.stringify({ task: form.task, max_iterations: parseInt(form.max_iterations) || 5 }),
        } : {},
      });
      onCreated();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to create agent');
    } finally { setLoading(false); }
  }

  return (
    <div
      style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.75)', display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 100, padding: 16 }}
      onClick={e => { if (e.target === e.currentTarget) onClose(); }}
    >
      <div style={{
        background: 'var(--surface-1)', border: '1px solid var(--line-2)',
        width: '100%', maxWidth: 520, maxHeight: '90vh', overflowY: 'auto',
        boxShadow: '0 24px 64px rgba(0,0,0,0.7)',
      }}>
        {/* Header */}
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: '1px solid var(--line-1)', background: 'var(--surface-0)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <Bot style={{ width: 14, height: 14, color: 'var(--cyan)' }} />
            <span style={{ fontFamily: 'var(--display)', fontSize: 14, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>
              DEPLOY NEW AGENT
            </span>
          </div>
          <button onClick={onClose} style={{ background: 'none', border: 'none', color: 'var(--ink-3)', cursor: 'pointer', padding: 4 }}>
            <X style={{ width: 14, height: 14 }} />
          </button>
        </div>

        <div style={{ padding: 16, display: 'flex', flexDirection: 'column', gap: 14 }}>
          {error && (
            <div style={{ padding: '8px 12px', background: 'rgba(255,61,85,0.1)', border: '1px solid rgba(255,61,85,0.3)', color: 'var(--crimson)', fontSize: 11 }}>
              {error}
            </div>
          )}

          {/* Presets */}
          <div>
            <div style={{ fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 6, fontWeight: 700 }}>QUICK PRESETS</div>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 6 }}>
              {PRESETS.map(p => (
                <button key={p.label} onClick={() => applyPreset(p)} style={{
                  padding: '6px 10px', background: 'var(--surface-0)', border: '1px solid var(--line-2)',
                  color: 'var(--ink-2)', fontSize: 10, fontFamily: 'var(--mono)', cursor: 'pointer',
                  textAlign: 'left', letterSpacing: '0.04em', transition: 'all 0.15s',
                }}>
                  <span style={{ color: 'var(--cyan)', marginRight: 4 }}>▸</span>{p.label}
                  <span style={{ display: 'block', fontSize: 8, color: 'var(--ink-4)', marginTop: 1 }}>{p.iter} iterations</span>
                </button>
              ))}
            </div>
          </div>

          {/* Name */}
          <div>
            <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>
              AGENT NAME <span style={{ color: 'var(--crimson)' }}>*</span>
            </label>
            <input
              className="op-input"
              value={form.name}
              onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              placeholder="my-agent-01"
              autoFocus
            />
          </div>

          {/* Description */}
          <div>
            <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>DESCRIPTION</label>
            <input
              className="op-input"
              value={form.description}
              onChange={e => setForm(f => ({ ...f, description: e.target.value }))}
              placeholder="What does this agent do?"
            />
          </div>

          {/* Task */}
          <div>
            <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>TASK</label>
            <input
              className="op-input"
              value={form.task}
              onChange={e => setForm(f => ({ ...f, task: e.target.value }))}
              placeholder="Analyze data and generate performance report"
            />
          </div>

          {/* Iterations */}
          <div>
            <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>MAX ITERATIONS</label>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(5, 1fr)', gap: 6 }}>
              {[2, 3, 5, 7, 10].map(n => (
                <button key={n} onClick={() => setForm(f => ({ ...f, max_iterations: String(n) }))} style={{
                  padding: '5px 0', fontFamily: 'var(--mono)', fontSize: 11, fontWeight: 700,
                  background: form.max_iterations === String(n) ? 'rgba(0,217,255,0.12)' : 'var(--surface-0)',
                  border: `1px solid ${form.max_iterations === String(n) ? 'rgba(0,217,255,0.4)' : 'var(--line-2)'}`,
                  color: form.max_iterations === String(n) ? 'var(--cyan)' : 'var(--ink-2)',
                  cursor: 'pointer', transition: 'all 0.15s',
                }}>{n}</button>
              ))}
            </div>
          </div>

          {/* Advanced toggle */}
          <button onClick={() => setShowAdvanced(s => !s)} style={{
            display: 'flex', alignItems: 'center', gap: 6,
            background: 'none', border: 'none', color: 'var(--ink-3)', cursor: 'pointer',
            fontSize: '8.5px', letterSpacing: '0.12em', fontWeight: 700, fontFamily: 'var(--mono)',
            padding: 0,
          }}>
            <ChevronDown style={{ width: 10, height: 10, transform: showAdvanced ? 'rotate(180deg)' : 'none', transition: 'transform 0.2s' }} />
            ADVANCED CONFIGURATION
          </button>

          {showAdvanced && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 10, padding: '10px 12px', background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
                <div>
                  <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>
                    <Cpu style={{ width: 9, height: 9, display: 'inline', marginRight: 4 }} />CPU LIMIT
                  </label>
                  <input className="op-input" value={form.cpu_limit} onChange={e => setForm(f => ({ ...f, cpu_limit: e.target.value }))} placeholder="1.0" />
                </div>
                <div>
                  <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>
                    <HardDrive style={{ width: 9, height: 9, display: 'inline', marginRight: 4 }} />MEMORY LIMIT
                  </label>
                  <input className="op-input" value={form.memory_limit} onChange={e => setForm(f => ({ ...f, memory_limit: e.target.value }))} placeholder="512m" />
                </div>
              </div>
              <div>
                <label style={{ display: 'block', fontSize: '8.5px', color: 'var(--ink-3)', letterSpacing: '0.14em', marginBottom: 5, fontWeight: 700 }}>
                  <Code style={{ width: 9, height: 9, display: 'inline', marginRight: 4 }} />DOCKER IMAGE
                </label>
                <input className="op-input" value={form.image} onChange={e => setForm(f => ({ ...f, image: e.target.value }))} />
              </div>
            </div>
          )}

          {/* Actions */}
          <div style={{ display: 'flex', gap: 8, paddingTop: 4 }}>
            <button onClick={onClose} className="op-btn op-btn-ghost" style={{ flex: 1 }}>CANCEL</button>
            <button onClick={handleSubmit} disabled={loading} className="op-btn op-btn-lime" style={{ flex: 2 }}>
              {loading ? '...' : '▸ DEPLOY AGENT'}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}