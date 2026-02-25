import clsx from 'clsx';

const map: Record<string, string> = {
  running: 'bg-green-500/20 text-green-400 border-green-500/30',
  stopped: 'bg-gray-500/20 text-gray-400 border-gray-500/30',
  created: 'bg-blue-500/20 text-blue-400 border-blue-500/30',
  error: 'bg-red-500/20 text-red-400 border-red-500/30',
  pending: 'bg-yellow-500/20 text-yellow-400 border-yellow-500/30',
  idle: 'bg-purple-500/20 text-purple-400 border-purple-500/30',
  completed: 'bg-teal-500/20 text-teal-400 border-teal-500/30',
};

export default function StatusBadge({ status }: { status: string }) {
  return (
    <span className={clsx('px-2 py-0.5 rounded-full text-xs font-medium border', map[status] || map.stopped)}>
      {status}
    </span>
  );
}