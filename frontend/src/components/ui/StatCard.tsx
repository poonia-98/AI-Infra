import { LucideIcon } from 'lucide-react';
import clsx from 'clsx';

interface Props {
  label: string;
  value: string | number;
  icon: LucideIcon;
  color?: 'blue' | 'green' | 'red' | 'purple' | 'yellow';
  sub?: string;
}

const colors = {
  blue: 'text-blue-400 bg-blue-400/10',
  green: 'text-green-400 bg-green-400/10',
  red: 'text-red-400 bg-red-400/10',
  purple: 'text-purple-400 bg-purple-400/10',
  yellow: 'text-yellow-400 bg-yellow-400/10',
};

export default function StatCard({ label, value, icon: Icon, color = 'blue', sub }: Props) {
  return (
    <div className="bg-[#111827] border border-gray-800 rounded-xl p-5 flex items-start gap-4">
      <div className={clsx('p-2.5 rounded-lg', colors[color])}>
        <Icon className="w-5 h-5" />
      </div>
      <div>
        <p className="text-xs text-gray-400 mb-1">{label}</p>
        <p className="text-2xl font-bold text-white">{value}</p>
        {sub && <p className="text-xs text-gray-500 mt-1">{sub}</p>}
      </div>
    </div>
  );
}