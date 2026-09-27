import { useEffect, useRef, useState } from 'react';
import { api } from '../api/client';
import type { JobETA, JobLogStreamEvent, JobStatus, StepTiming } from '../types';
import StepTimingsView from './StepTimingsView';
import JobMetricsView from './JobMetricsView';

// terminalStatuses mirrors the backend JobStatus.Terminal() set: for these,
// the captured full log (GET /log) is the only content — no live stream.
const terminalStatuses: JobStatus[] = ['done', 'failed', 'cancelled'];

// JobLogDialog fetches the raw text/plain job log (GET /api/jobs/{id}/log)
// only when the operator opens it — the jobs list deliberately carries no
// full_log to avoid bloating the queue payload. Step timings (when present)
// render above the log so the operator sees the breakdown and the raw log in
// one view; the Jobs page has no separate expanded detail row today, so the
// dialog is the single home for both (noted fallback in the B3 spec).
//
// Live mode: when jobStatus is non-terminal, the dialog also opens the SSE
// stream (GET /api/jobs/{id}/log/stream) and renders the live step/progress/
// tail above the (still absent) full log. The full log only exists after
// completion, so while live the stream IS the content; on the final event
// the dialog refetches the captured log so the operator sees the complete
// run without reopening.
export default function JobLogDialog({
  jobId,
  jobLabel,
  onClose,
  stepTimings,
  jobStatus,
  metrics,
}: {
  jobId: number;
  jobLabel: string;
  onClose: () => void;
  stepTimings?: StepTiming[];
  jobStatus?: JobStatus;
  metrics?: Record<string, number>;
}) {
  const [log, setLog] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);
  const [live, setLive] = useState<JobLogStreamEvent | null>(null);
  // ETA for live jobs, refreshed with each heartbeat-driven stream event.
  // null = not fetched (terminal job) or no estimate yet.
  const [eta, setEta] = useState<JobETA | null>(null);
  const preRef = useRef<HTMLPreElement>(null);

  // fetchFullLog is shared between initial load and the post-final refetch.
  function fetchFullLog(): Promise<void> {
    return api
      .getJobLog(jobId)
      .then((text) => {
        setLog(text);
        setError(null);
      })
      .catch((e) => {
        // A running job has no captured log yet (404 "no log recorded") —
        // not an error worth showing while the live stream is active.
        if (jobStatus && !terminalStatuses.includes(jobStatus)) return;
        setError(e instanceof Error ? e.message : 'failed to load log');
      })
      .finally(() => setBusy(false));
  }

  useEffect(() => {
    setBusy(true);
    fetchFullLog();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobId]);

  // Live SSE stream while the job is non-terminal. The server closes the
  // stream on the final event; we then refetch the captured full log so the
  // dialog shows the complete run. AbortController cancels on unmount or
  // when the job flips terminal (dialog re-render with new status).
  useEffect(() => {
    if (!jobStatus || terminalStatuses.includes(jobStatus)) return;
    const ac = new AbortController();
    api
      .streamJobLog(
        jobId,
        (ev) => {
          setLive(ev);
          if (ev.type === 'final') {
            setBusy(true);
            fetchFullLog();
            setEta(null);
          } else {
            // Refresh the ETA with each progress event (heartbeat cadence).
            // Best-effort: a failed fetch keeps the previous estimate.
            api.jobETA(jobId).then(setEta).catch(() => undefined);
          }
        },
        ac.signal,
      )
      .catch(() => {
        /* aborted on unmount or stream closed by server — not an error */
      });
    return () => ac.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobId, jobStatus]);

  // Auto-scroll the live tail so the operator always sees the newest lines.
  useEffect(() => {
    if (live?.type === 'progress' && preRef.current) {
      preRef.current.scrollTop = preRef.current.scrollHeight;
    }
  }, [live]);


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
        <JobMetricsView metrics={metrics} />
        {live && live.type === 'progress' && (
          <>
            <h4>
              Live tail {live.step ? `— ${live.step}` : ''}
              {typeof live.progress === 'number' && live.progress > 0
                ? ` (${live.progress.toFixed(0)}%)`
                : ''}
              {eta && eta.eta_sec >= 0 && eta.samples >= 2 && (
                <span className="muted" style={{ fontWeight: 'normal', fontSize: '0.85em' }}>
                  {' '}— ~{humanizeEta(eta.eta_sec)} left (avg {humanizeEta(eta.avg_sec)} over {eta.samples} runs)
                </span>
              )}
            </h4>
            <pre className="job-log-pre" ref={preRef}>
              {live.log_tail ?? ''}
            </pre>
          </>
        )}
        {live && live.type === 'final' && (
          <p className="muted">
            Job finished ({live.status}
            {live.error ? `: ${live.error}` : ''}) — loading captured log…
          </p>
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

// humanizeEta formats seconds as a compact "1h 23m" / "45m" / "30s" label.
function humanizeEta(sec: number): string {
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return `${s}s`;
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return h > 0 ? `${h}h ${m}m` : `${m}m`;
}
