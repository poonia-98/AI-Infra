'use client';
import { useEffect, useRef } from 'react';
import { Log } from '@/lib/api';
import clsx from 'clsx';
import { formatDistanceToNow } from 'date-fns';

const levelColors: Record<string, string> = {
  error: 'text-red-400',
  warn: 'text-yellow-400',
  warning: 'text-yellow-400',
  info: 'text-blue-400',
  debug: 'text-gray-500',
};

interface Props {
  logs: Log[];
  autoScroll?: boolean;
}

export default function LogViewer({ logs, autoScroll = true }: Props) {
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (autoScroll) {
      bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
    }
  }, [logs, autoScroll]);

  return (
    <div className="font-mono text-xs bg-[#0a0e1a] rounded-lg border border-gray-800 overflow-auto h-96 scrollbar-thin p-3 space-y-0.5">
      {logs.length === 0 && (
        <div className="text-gray-600 italic">No logs yet...</div>
      )}
      {[...logs].reverse().map((log) => (
        <div key={log.id} className="flex gap-3 hover:bg-gray-800/30 px-1 py-0.5 rounded">
          <span className="text-gray-600 shrink-0 w-28">
            {formatDistanceToNow(new Date(log.timestamp), { addSuffix: true })}
          </span>
          <span className={clsx('shrink-0 w-12 uppercase font-bold', levelColors[log.level] || 'text-gray-400')}>
            {log.level}
          </span>
          {log.source && (
            <span className="text-purple-400 shrink-0">[{log.source}]</span>
          )}
          <span className="text-gray-300 break-all">{log.message}</span>
        </div>
      ))}
      <div ref={bottomRef} />
    </div>
  );
}