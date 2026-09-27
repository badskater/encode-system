import { useCallback, useEffect, useState } from 'react';
import type { AuditEvent } from '../types';
import { api } from '../api/client';

// AuditPage renders the audit log newest-first: who did what, when, to
// which object, plus the (already secret-scrubbed) detail snippet. A
// client-side action filter keeps the table scannable without a server
// query param — 200 rows is small enough to filter locally.
export default function AuditPage() {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [filter, setFilter] = useState('');

  const load = useCallback(() => {
    setLoading(true);
    api
      .listAudit(200)
      .then((rows) => setEvents(rows))
      .catch((err) => setError(String(err)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(load, [load]);

  const rows = filter ? events.filter((e) => e.action.includes(filter) || e.actor.includes(filter)) : events;

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12 }}>
        <h2 style={{ margin: 0 }}>Audit log</h2>
        <input
          aria-label="Filter audit events"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter action or actor…"
          style={{ width: 220 }}
        />
        <button onClick={load} aria-label="Refresh audit log">
          Refresh
        </button>
      </div>
      {error && <p style={{ color: 'var(--danger)' }}>{error}</p>}
      {loading && <p className="muted">Loading…</p>}
      {!loading && !error && rows.length === 0 && <p className="muted">No audit events yet.</p>}
      {!loading && rows.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>When</th>
              <th>Actor</th>
              <th>Action</th>
              <th>Object</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((ev) => (
              <tr key={ev.id}>
                <td style={{ whiteSpace: 'nowrap' }}>{formatTime(ev.at)}</td>
                <td>{ev.actor}</td>
                <td>
                  <code>{ev.action}</code>
                </td>
                <td>{ev.object}</td>
                <td style={{ maxWidth: 420, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={ev.detail}>
                  {ev.detail || <span className="muted">—</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// formatTime renders a server timestamp as local HH:MM:SS on MM-DD. The
// server stores datetime('now') (UTC) without a zone marker, so append Z
// before parsing to avoid the browser treating it as local time.
function formatTime(at: string): string {
  if (!at) return '';
  const iso = at.endsWith('Z') || at.includes('+') ? at : at.replace(' ', 'T') + 'Z';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return at;
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
