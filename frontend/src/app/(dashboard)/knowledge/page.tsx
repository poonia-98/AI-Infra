'use client';
import { useEffect, useState } from 'react';
import { api, KnowledgeBase } from '@/lib/api';

export default function KnowledgePage() {
  const [bases, setBases] = useState<KnowledgeBase[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<any[]>([]);
  const [searching, setSearching] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.knowledge.bases().catch(() => []).then(b => { setBases(b); setLoading(false); });
  }, []);

  async function search() {
    if (!selected || !query.trim()) return;
    setSearching(true);
    try {
      const res = await api.knowledge.search(selected, query);
      setResults(res);
    } catch { setResults([]); }
    setSearching(false);
  }

  return (
    <div style={{ padding: '20px 24px' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontFamily: 'var(--display)', fontSize: 20, fontWeight: 700, color: 'var(--ink-0)', letterSpacing: '0.08em' }}>KNOWLEDGE BASES</div>
        <div style={{ fontSize: 9, color: 'var(--ink-4)', letterSpacing: '0.12em', marginTop: 3 }}>VECTOR MEMORY · SEMANTIC SEARCH · EMBEDDINGS</div>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '280px 1fr', gap: 16 }}>
        {/* Base list */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--cyan)', fontWeight: 700 }}>KNOWLEDGE BASES</span>
          </div>
          <div>
            {loading ? <div style={{ fontSize: 9, color: 'var(--ink-4)', padding: 16 }}>LOADING...</div>
              : bases.length === 0 ? (
                <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '32px 16px' }}>No knowledge bases created</div>
              ) : bases.map(base => (
                <div
                  key={base.id}
                  onClick={() => setSelected(base.id)}
                  style={{
                    padding: '10px 14px', cursor: 'pointer',
                    borderBottom: '1px solid var(--line-0)',
                    background: selected === base.id ? 'rgba(0,217,255,0.06)' : 'transparent',
                    borderLeft: `2px solid ${selected === base.id ? 'var(--cyan)' : 'transparent'}`,
                  }}
                >
                  <div style={{ fontSize: 10, color: 'var(--ink-0)', fontWeight: 600, marginBottom: 3 }}>{base.name}</div>
                  <div style={{ fontSize: 8.5, color: 'var(--ink-4)', display: 'flex', gap: 10 }}>
                    <span>{base.chunk_count} chunks</span>
                    <span>{base.embedding_model}</span>
                  </div>
                </div>
              ))}
          </div>
        </div>

        {/* Search panel */}
        <div style={{ background: 'var(--surface-0)', border: '1px solid var(--line-1)' }}>
          <div style={{ padding: '8px 14px', borderBottom: '1px solid var(--line-1)' }}>
            <span style={{ fontSize: 9, letterSpacing: '0.2em', color: 'var(--violet)', fontWeight: 700 }}>SEMANTIC SEARCH</span>
          </div>
          <div style={{ padding: 16 }}>
            <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
              <input
                value={query}
                onChange={e => setQuery(e.target.value)}
                onKeyDown={e => e.key === 'Enter' && search()}
                placeholder={selected ? 'Enter semantic query...' : 'Select a knowledge base first'}
                disabled={!selected}
                style={{
                  flex: 1, padding: '8px 12px',
                  background: 'var(--surface-1)', border: '1px solid var(--line-2)',
                  color: 'var(--ink-0)', fontFamily: 'var(--mono)', fontSize: 11, outline: 'none',
                }}
              />
              <button
                onClick={search}
                disabled={!selected || !query.trim() || searching}
                style={{
                  padding: '8px 16px', background: 'rgba(168,85,247,0.1)',
                  border: '1px solid rgba(168,85,247,0.3)', color: 'var(--violet)',
                  fontFamily: 'var(--mono)', fontSize: 9, cursor: 'pointer', letterSpacing: '0.1em',
                }}
              >
                {searching ? '◌ SEARCHING' : '→ SEARCH'}
              </button>
            </div>
            <div>
              {results.length === 0 && query && !searching && (
                <div style={{ fontSize: 9, color: 'var(--ink-4)', textAlign: 'center', padding: '24px 0' }}>No results found</div>
              )}
              {results.map((r, i) => (
                <div key={i} style={{ padding: '10px 12px', marginBottom: 8, background: 'var(--surface-1)', border: '1px solid var(--line-1)', borderLeft: '3px solid var(--violet)' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 6 }}>
                    <span style={{ fontSize: 8.5, color: 'var(--violet)', letterSpacing: '0.1em' }}>CHUNK #{i + 1}</span>
                    <span style={{ fontSize: 8.5, color: 'var(--lime)' }}>
                      {r.score ? `${(r.score * 100).toFixed(1)}% match` : ''}
                    </span>
                  </div>
                  <p style={{ fontSize: 10, color: 'var(--ink-1)', lineHeight: 1.6 }}>{r.content || r.text}</p>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}