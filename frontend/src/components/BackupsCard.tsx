import { useCallback, useEffect, useState } from 'react';
import type { BackupInfo, BackupStatus } from '../types';
import { api } from '../api/client';
import { fmtTime, timeAgo } from './helpers';

// BackupsCard manages controller DB snapshots: toggle/interval for the
// scheduler, manual "Back up now", and list/download/delete of snapshots.
// Downloads use fetch + blob URL because the API authenticates via the
// Authorization header, which a plain <a href> cannot send.
export default function BackupsCard() {
  const [status, setStatus] = useState<BackupStatus | null>(null);
  const [snapshots, setSnapshots] = useState<BackupInfo[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [enabled, setEnabled] = useState(false);
  const [everyH, setEveryH] = useState(6);
  // restoreConfirm holds the snapshot name awaiting the operator's confirm
  // click. null = no restore dialog open. The 409 "active jobs" error from
  // the backend surfaces inline in the dialog.
  const [restoreConfirm, setRestoreConfirm] = useState<string | null>(null);
  const [restoreMsg, setRestoreMsg] = useState('');
  const [restoreNotice, setRestoreNotice] = useState('');

  const load = useCallback(() => {
    api
      .backupStatus()
      .then((r) => {
        setStatus(r.status);
        setSnapshots(r.snapshots ?? []);
        setEnabled(!!r.status.enabled);
        setEveryH(Math.round((r.status.every_seconds ?? 21600) / 3600));
      })
      .catch((err) => setError(String(err)));
  }, []);

  useEffect(load, [load]);

  async function takeNow() {
    setBusy(true);
    setError('');
    try {
      await api.backupNow();
      load();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function saveSchedule() {
    setBusy(true);
    setError('');
    try {
      const st = await api.backupSettings(enabled, everyH * 3600);
      setStatus(st);
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function remove(name: string) {
    setBusy(true);
    setError('');
    try {
      await api.deleteBackup(name);
      load();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function download(name: string) {
    setError('');
    try {
      const blob = await api.downloadBackup(name);
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = name;
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      setError(String(err));
    }
  }

  // restore swaps a snapshot into the live DB. SQLite restarts the process
  // (Docker restart policy); PG replays in-place. A 409 means active jobs
  // are in flight — the message surfaces in the confirm dialog so the
  // operator knows to drain/cancel first.
  async function restore(name: string) {
    setBusy(true);
    setRestoreMsg('');
    try {
      const res = await api.restoreBackup(name);
      setRestoreNotice(
        res.restart
          ? 'Restored — the controller is restarting on the restored database.'
          : 'Restored — the database was replayed in place (no restart needed).',
      );
      setRestoreConfirm(null);
      load();
    } catch (err) {
      setRestoreMsg(String(err instanceof Error ? err.message.replace(/^\d+:\s*/, '') : err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h3 style={{ marginTop: 0 }}>Controller DB backups</h3>
      <p className="muted">
        Snapshots of <code>encode.db</code> taken with SQLite VACUUM INTO —
        consistent, standalone files. Restore = stop the controller, replace{' '}
        <code>encode.db</code> with a snapshot, start. The scheduler keeps the
        newest 24 scheduled snapshots; manual snapshots are never auto-pruned.
      </p>
      {error && <div className="error-box">{error}</div>}
      {restoreNotice && (
        <div className="card" style={{ borderColor: 'var(--green)', marginBottom: 14 }}>
          {restoreNotice}
        </div>
      )}

      <div className="toolbar" style={{ alignItems: 'flex-end', margin: '12px 0' }}>
        <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <input
            type="checkbox"
            aria-label="Scheduled backups enabled"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <span>Scheduled snapshots</span>
        </label>
        <label>
          <span style={{ display: 'block', marginBottom: 2 }}>Every (hours)</span>
          <input
            aria-label="Backup interval hours"
            type="number"
            min={1}
            max={24}
            style={{ width: 80 }}
            value={everyH}
            onChange={(e) => setEveryH(Number(e.target.value))}
          />
        </label>
        <button disabled={busy} onClick={saveSchedule}>
          Save schedule
        </button>
        <button className="btn primary" disabled={busy} onClick={takeNow} aria-label="Back up now">
          Back up now
        </button>
      </div>

      {status && (
        <p className="muted" style={{ fontSize: 12 }}>
          {status.last_run ? <>last run {timeAgo(status.last_run)} · </> : 'never run · '}
          {status.enabled && status.next_run ? <>next {fmtTime(status.next_run)} · </> : null}
          {status.last_error ? <span style={{ color: 'var(--danger)' }}>last error: {status.last_error}</span> : 'scheduler healthy'}
        </p>
      )}

      {snapshots.length === 0 ? (
        <p className="muted">No snapshots yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Size</th>
              <th>Taken</th>
              <th />
              <th />
              <th />
            </tr>
          </thead>
          <tbody>
            {snapshots.map((snap) => (
              <tr key={snap.name}>
                <td>
                  <code>{snap.name}</code>{' '}
                  {snap.scheduled && <span className="muted">(sched)</span>}
                </td>
                <td>{(snap.size_bytes / 1024).toFixed(0)} KiB</td>
                <td>{fmtTime(snap.created_at)}</td>
                <td>
                  <button
                    aria-label={`Restore ${snap.name}`}
                    disabled={busy}
                    onClick={() => { setRestoreConfirm(snap.name); setRestoreMsg(''); }}
                    title="Stop the controller, replace encode.db with this snapshot, restart"
                  >
                    Restore
                  </button>
                </td>
                <td>
                  <button aria-label={`Download ${snap.name}`} onClick={() => download(snap.name)}>
                    Download
                  </button>
                </td>
                <td>
                  <button aria-label={`Delete ${snap.name}`} disabled={busy} onClick={() => remove(snap.name)}>
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {restoreConfirm && (
        <div className="card" style={{ borderColor: 'var(--yellow)' }}>
          <strong>Restore {restoreConfirm}?</strong>
          <p className="muted" style={{ fontSize: 12, margin: '6px 0' }}>
            This replaces the live database with this snapshot. SQLite: the
            controller restarts (Docker policy). PostgreSQL: the dump replays
            in place. Active jobs must be drained or cancelled first.
          </p>
          {restoreMsg && <div className="error-box">{restoreMsg}</div>}
          <div className="toolbar" style={{ marginBottom: 0 }}>
            <button
              className="btn danger"
              disabled={busy}
              onClick={() => restore(restoreConfirm)}
            >
              {busy ? 'Restoring…' : 'Confirm restore'}
            </button>
            <button disabled={busy} onClick={() => { setRestoreConfirm(null); setRestoreMsg(''); }}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
