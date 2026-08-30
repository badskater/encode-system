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

// parseTime normalizes backend timestamps ("2006-01-02 15:04:05" UTC, no
// T/Z) into a Date. Shared by fmtTime/timeAgo AND any code that compares
// timestamps against Date.now() — a raw new Date() on the space shape is a
// Safari trap (Invalid Date) and parses as LOCAL time in V8, off by the
// UTC offset. Returns null on missing/invalid input.
export function parseTime(iso?: string | null): Date | null {
  if (!iso) return null;
  const d = new Date(iso.endsWith('Z') || iso.includes('T') ? iso : iso + 'Z');
  return Number.isNaN(d.getTime()) ? null : d;
}

export function fmtTime(iso?: string | null) {
  const d = parseTime(iso);
  if (!d) return iso || '—';
  return d.toLocaleString();
}

export function timeAgo(iso?: string | null) {
  const d = parseTime(iso);
  if (!d) return iso || 'never';
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  return `${Math.floor(s / 3600)}h ago`;
}
