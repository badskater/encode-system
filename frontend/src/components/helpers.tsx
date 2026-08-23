import type { JobStatus, NodeStatus } from '../types';

export function jobBadge(status: JobStatus) {
  const cls =
    status === 'done' ? 'green'
    : status === 'failed' ? 'red'
    : status === 'running' ? 'blue'
    : status === 'assigned' ? 'yellow'
    : 'gray';
  return <span className={`badge ${cls}`}>{status}</span>;
}

export function nodeBadge(status: NodeStatus, online: boolean) {
  if (!online && status !== 'offline') status = 'offline';
  const cls =
    status === 'idle' ? 'green'
    : status === 'busy' ? 'blue'
    : status === 'reboot_pending' ? 'yellow'
    : 'gray';
  return <span className={`badge ${cls}`}>{status}</span>;
}

export function fmtTime(iso?: string | null) {
  if (!iso) return '—';
  const d = new Date(iso.endsWith('Z') || iso.includes('T') ? iso : iso + 'Z');
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

export function timeAgo(iso?: string | null) {
  if (!iso) return 'never';
  const d = new Date(iso.endsWith('Z') || iso.includes('T') ? iso : iso + 'Z');
  if (Number.isNaN(d.getTime())) return iso;
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  return `${Math.floor(s / 3600)}h ago`;
}
