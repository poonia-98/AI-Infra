'use client';
import { LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer, Legend } from 'recharts';
import { Metric } from '@/lib/api';
import { format } from 'date-fns';

interface Props {
  metrics: Metric[];
  metricNames?: string[];
}

const COLORS = ['#4f6ef7', '#34d399', '#f59e0b', '#ec4899', '#8b5cf6'];

export default function MetricsChart({ metrics, metricNames }: Props) {
  const names = metricNames || Array.from(new Set(metrics.map(m => m.metric_name)));

  // Group by timestamp bucket (15s)
  const buckets = new Map<number, Record<string, number>>();
  metrics.forEach(m => {
    const ts = Math.floor(new Date(m.timestamp).getTime() / 15000) * 15000;
    if (!buckets.has(ts)) buckets.set(ts, { time: ts });
    const bucket = buckets.get(ts)!;
    bucket[m.metric_name] = m.metric_value;
  });

  const data = Array.from(buckets.entries())
    .sort(([a], [b]) => a - b)
    .slice(-30)
    .map(([, v]) => ({ ...v, label: format(new Date(v.time), 'HH:mm:ss') }));

  if (data.length === 0) {
    return (
      <div className="h-48 flex items-center justify-center text-gray-500 text-sm">
        No metrics data yet
      </div>
    );
  }

  return (
    <ResponsiveContainer width="100%" height={200}>
      <LineChart data={data} margin={{ top: 5, right: 10, bottom: 5, left: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke="#1f2937" />
        <XAxis dataKey="label" stroke="#6b7280" tick={{ fontSize: 10 }} />
        <YAxis stroke="#6b7280" tick={{ fontSize: 10 }} />
        <Tooltip
          contentStyle={{ backgroundColor: '#111827', border: '1px solid #374151', borderRadius: '8px' }}
          labelStyle={{ color: '#9ca3af' }}
        />
        <Legend wrapperStyle={{ fontSize: 11 }} />
        {names.map((name, i) => (
          <Line
            key={name}
            type="monotone"
            dataKey={name}
            stroke={COLORS[i % COLORS.length]}
            strokeWidth={2}
            dot={false}
            activeDot={{ r: 4 }}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  );
}