import { useEffect, useState } from 'react';
import { api } from '../api/client';
import type { StepTiming } from '../types';
import StepTimingsView from './StepTimingsView';

// JobLogDialog fetches the raw text/plain job log (GET /api/jobs/{id}/log)
// only when the operator opens it — the jobs list deliberately carries no
// full_log to avoid bloating the queue payload. Step timings (when present)
// render above the log so the operator sees the breakdown and the raw log in
// one view; the Jobs page has no separate expanded detail row today, so the
// dialog is the single home for both (noted fallback in the B3 spec).
export default function JobLogDialog({
  jobId,
  jobLabel,
  onClose,
  stepTimings,
}: {
  jobId: number;
  jobLabel: string;
  onClose: () => void;
  stepTimings?: StepTiming[];
}) {
  const [log, setLog] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);

  useEffect(() => {
    let mounted = true;
    setBusy(true);
    api
      .getJobLog(jobId)
      .then((text) => {
        if (mounted) {
          setLog(text);
          setError(null);
        }
      })
      .catch((e) => {
        if (mounted) setError(e instanceof Error ? e.message : 'failed to load log');
      })
      .finally(() => {
        if (mounted) setBusy(false);
      });
    return () => {
      mounted = false;
    };
  }, [jobId]);

  // Escape closes the dialog when not loading (matches the existing dialogs'
  // backdrop-close-on-not-busy behavior). The page has no global Escape
  // handler, so we attach one here.
  useEffect(() => {
    if (busy) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [busy, onClose]);

  function download() {
    if (log === null) return;
    const blob = new Blob([log], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `job-${jobId}.log`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
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
        style={{ maxWidth: 900 }}
      >
        <h3 style={{ marginTop: 0 }}>
          Job #{jobId} — {jobLabel}
        </h3>
        {busy && <p className="muted">Loading log…</p>}
        {error && <div className="error-box">{error}</div>}
        {stepTimings && stepTimings.length > 0 && (
          <StepTimingsView timings={stepTimings} />
        )}
        {log !== null && (
          <>
            <h4>Full log</h4>
            <pre className="job-log-pre">{log}</pre>
          </>
        )}
        <div className="toolbar">
          <button
            className="btn"
            onClick={download}
            disabled={log === null}
          >
            Download
          </button>
          <button className="btn" onClick={onClose} disabled={busy}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
}
