import { useState } from 'react';
import { api } from '../api/client';
import type { Series } from '../types';

// DeleteSeriesDialog confirms a registry-only series deletion. It shows the
// series' progress counts (already fetched by GET /api/series, derived from
// the jobs table) so the operator sees what history is attached before
// confirming. Jobs, history and files survive the delete; if the series
// folder still exists on the scripts share with ready episodes, the scanner
// re-registers it on the next scan — the dialog says so explicitly.
export default function DeleteSeriesDialog({
  series,
  onClose,
  onDeleted,
}: {
  series: Series;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const done = series.episodes_done ?? 0;
  const total = series.episodes_total ?? 0;
  const jobs = series.jobs ?? 0;

  async function submit() {
    if (busy) return; // Enter key-repeat / double-click guard
    setError('');
    setBusy(true);
    try {
      await api.deleteSeries(series.id);
      onDeleted();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Series deletion failed');
      setBusy(false);
    }
  }

  return (
    <div
      className="modal-backdrop"
      onClick={() => !busy && onClose()}
      role="dialog"
      aria-modal="true"
    >
      <div
        className="card modal"
        onClick={(e) => e.stopPropagation()}
        style={{ maxWidth: 460 }}
      >
        <h3>Delete series</h3>
        <p>
          Remove <strong>{series.name}</strong> from the series registry?
        </p>
        <div style={{ fontSize: 13, lineHeight: 1.7 }}>
          <div>
            <span className="muted">Progress: </span>
            {done}/{total} episodes encoded
          </div>
          <div>
            <span className="muted">Jobs in history: </span>
            {jobs}
          </div>
        </div>
        <p className="muted">
          Only the registry row is removed — jobs, history and files on the
          shares are kept. If the series folder still exists on the scripts
          share with ready episodes, the scanner re-registers it on the next
          scan; remove or rename the folder to make the deletion stick.
        </p>
        {error && <div className="error-box">{error}</div>}
        <div className="toolbar">
          <button className="btn danger" disabled={busy} onClick={submit}>
            {busy ? 'Deleting…' : 'Delete series'}
          </button>
          <button className="btn" disabled={busy} onClick={onClose}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}