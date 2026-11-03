import { useEffect, useRef, useState } from 'react';
import { api } from '../api/client';
import type { Flow, Job, JobStatus, Node } from '../types';
import { usePolling } from '../hooks/usePolling';
import { jobBadge, fmtTime, parseTime } from '../components/helpers';
import JobLogDialog from '../components/JobLogDialog';
import { useIsAdmin } from '../hooks/useRole';
import StepTimingsView from '../components/StepTimingsView';

const FILTERS: (JobStatus | '')[] = ['', 'pending', 'assigned', 'running', 'done', 'failed'];

// TERMINAL is the set of statuses for which a full log is available: a log
// only exists once the agent has finished (done/failed) or the job was
// cancelled out of the queue. Pending/assigned/running jobs have no log yet.
const TERMINAL: ReadonlySet<JobStatus> = new Set(['done', 'failed', 'cancelled']);

// JobsPage lists the queue with filters, retry/cancel actions, and a detail
// view showing the captured log tail and step-timing breakdown.
export default function JobsPage() {
  const [filter, setFilter] = useState<JobStatus | ''>('');
  const [selected, setSelected] = useState<Job | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [logJob, setLogJob] = useState<Job | null>(null);
  // Bulk selection: job ids checked for a bulk retry/cancel. Kept as a Set
  // for O(1) toggles; cleared after each bulk op and when the filter changes
  // (a selection that spans filters would act on rows the operator can no
  // longer see — dangerous for destructive ops).
  const [checked, setChecked] = useState<Set<number>>(new Set());
  const [bulkMsg, setBulkMsg] = useState<string | null>(null);
  const isAdmin = useIsAdmin();
  // deleteConfirm holds the job id awaiting the operator's confirm click
  // for a single-job delete. null = no dialog open. The 409 "only terminal
  // jobs can be deleted" error surfaces inline in the dialog.
  const [deleteConfirm, setDeleteConfirm] = useState<Job | null>(null);
  const [deleteMsg, setDeleteMsg] = useState('');

  const { data: jobs } = usePolling<Job[]>(
    () => api.jobs(filter || undefined),
    4000,
  );
  const { data: nodes } = usePolling<Node[]>(() => api.nodes(), 30000);
  const { data: flows } = usePolling<Flow[]>(() => api.flows(), 30000);

  // Deep-link consumption (Phase F2): Discord alert messages carry a link
  // like /jobs?job=42. On mount we read that param once; when the polled job
  // list arrives and contains that job, we auto-open its log dialog (same
  // setLogJob path the Log button uses). The consumed ref prevents
  // re-triggering on every poll tick — the dialog opens exactly once.
  const deepLinkConsumed = useRef(false);
  useEffect(() => {
    if (deepLinkConsumed.current) return;
    if (!jobs || jobs.length === 0) return;
    const params = new URLSearchParams(window.location.search);
    const raw = params.get('job');
    if (!raw) {
      deepLinkConsumed.current = true; // no param — mark consumed either way
      return;
    }
    const id = Number(raw);
    if (!Number.isFinite(id)) {
      deepLinkConsumed.current = true;
      return;
    }
    const found = jobs.find((j) => j.id === id);
    if (found) {
      deepLinkConsumed.current = true;
      setLogJob(found);
    }
    // If not found yet, leave consumed=false so a later poll can match it
    // (the job may still be loading). Once matched, it's consumed for good.
  }, [jobs]);

  const nodeName = (id?: number) => nodes?.find((n) => n.id === id)?.name ?? '—';
  const flowName = (id: number) => flows?.find((f) => f.id === id)?.name ?? `#${id}`;

  async function changeFlow(id: number, flowId: number) {
    try {
      await api.patchJob(id, { flow_id: flowId });
      setError(null);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    }
  }

  // changePriority PATCHes {priority} on a pending job, mirroring changeFlow.
  async function changePriority(id: number, priority: number) {
    try {
      await api.patchJob(id, { priority });
      setError(null);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    }
  }

  async function retry(id: number) {
    try {
      await api.retryJob(id);
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }

  async function cancel(id: number) {
    try {
      await api.cancelJob(id);
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }

  // deleteJob removes a terminal job after confirmation. The 409
  // "only terminal jobs can be deleted" error surfaces inline in the
  // confirm dialog so the operator knows why it was refused.
  async function deleteJobConfirm(job: Job) {
    setDeleteMsg('');
    try {
      await api.deleteJob(job.id);
      setDeleteConfirm(null);
      setError(null);
    } catch (e) {
      setDeleteMsg(String(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : e));
    }
  }

  // bulkDelete runs the bulk delete action over the checked ids.
  async function bulkDelete() {
    const ids = [...checked];
    if (ids.length === 0) return;
    try {
      const res = await api.bulkJobs('delete', ids);
      setBulkMsg(
        `delete: ${res.affected} affected` +
          (res.skipped.length > 0 ? `, ${res.skipped.length} skipped` : ''),
      );
      setChecked(new Set());
      setError(null);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    }
  }

  // bulkSetPriority prompts for a priority value and applies it to the
  // checked pending jobs via the bulk set_priority action.
  async function bulkSetPriority() {
    const ids = [...checked];
    if (ids.length === 0) return;
    const raw = prompt('Set priority for selected jobs (0=Normal, 1=High):', '1');
    if (raw === null) return;
    const priority = Number(raw);
    if (!Number.isFinite(priority) || priority < 0) {
      setError('Priority must be a non-negative number');
      return;
    }
    try {
      const res = await api.bulkJobs('set_priority', ids, { priority });
      setBulkMsg(
        `set_priority: ${res.affected} affected` +
          (res.skipped.length > 0 ? `, ${res.skipped.length} skipped` : ''),
      );
      setChecked(new Set());
      setError(null);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    }
  }

  // bulkSetFlow prompts for a flow id and applies it to the checked
  // pending jobs via the bulk set_flow action.
  async function bulkSetFlow() {
    const ids = [...checked];
    if (ids.length === 0) return;
    const options = (flows ?? []).map((f) => `${f.id}: ${f.name}${f.is_default ? ' (default)' : ''}`).join('\n');
    const raw = prompt(`Set flow for selected jobs:\n${options}`, '');
    if (raw === null) return;
    const flowId = Number(raw);
    if (!Number.isFinite(flowId) || flowId <= 0) {
      setError('Flow id must be a positive number');
      return;
    }
    try {
      const res = await api.bulkJobs('set_flow', ids, { flow_id: flowId });
      setBulkMsg(
        `set_flow: ${res.affected} affected` +
          (res.skipped.length > 0 ? `, ${res.skipped.length} skipped` : ''),
      );
      setChecked(new Set());
      setError(null);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    }
  }

  // bulk runs one bulk action over the checked ids, then reports the
  // affected/skipped counts and clears the selection. The server's guard
  // does the real work (skipped = wrong-state or missing rows), so the UI
  // never pre-filters by status.
  async function bulk(action: 'retry' | 'cancel') {
    const ids = [...checked];
    if (ids.length === 0) return;
    try {
      const res = await api.bulkJobs(action, ids);
      setBulkMsg(
        `${action}: ${res.affected} affected` +
          (res.skipped.length > 0 ? `, ${res.skipped.length} skipped` : ''),
      );
      setChecked(new Set());
      setError(null);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    }
  }

  // toggleCheck flips one row; selectAllChecked mirrors "every visible row
  // is checked" for the header checkbox (indeterminate state handled inline).
  function toggleCheck(id: number) {
    setChecked((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  const visibleIds = (jobs ?? []).map((j) => j.id);
  const allChecked = visibleIds.length > 0 && visibleIds.every((id) => checked.has(id));
  function toggleSelectAll() {
    setChecked(allChecked ? new Set() : new Set(visibleIds));
  }

  return (
    <>
      <h2>Jobs</h2>
      {error && <div className="error-box">{error}</div>}

      <div className="toolbar">
        <label className="muted">Filter:</label>
        <select
          value={filter}
          onChange={(e) => {
            setFilter(e.target.value as JobStatus | '');
            // Filter change clears the bulk selection: acting on rows the
            // operator can no longer see would be surprising for a
            // destructive op.
            setChecked(new Set());
          }}
        >
          {FILTERS.map((f) => (
            <option key={f} value={f}>
              {f || 'all'}
            </option>
          ))}
        </select>
        {checked.size > 0 && (
          <>
            <span className="muted">{checked.size} selected</span>
            {isAdmin && (
              <>
                <button className="btn" onClick={() => bulk('retry')}>
                  Retry selected
                </button>
                <button className="btn danger" onClick={() => bulk('cancel')}>
                  Cancel selected
                </button>
                <button className="btn danger" onClick={bulkDelete}>
                  Delete selected
                </button>
                <button className="btn" onClick={bulkSetPriority}>
                  Set priority
                </button>
                <button className="btn" onClick={bulkSetFlow}>
                  Set flow
                </button>
              </>
            )}
          </>
        )}
      </div>
      {bulkMsg && <p className="muted">{bulkMsg}</p>}

      <table>
        <thead>
          <tr>
            {isAdmin && (
              <th>
                <input
                  type="checkbox"
                  checked={allChecked}
                  onChange={toggleSelectAll}
                  aria-label="Select all jobs"
                  title="Select all visible jobs"
                />
              </th>
            )}
            <th>#</th>
            <th>Series</th>
            <th>Ep</th>
            <th>Script</th>
            <th>Status</th>
            <th>Flow</th>
            <th>Priority</th>
            <th>Node</th>
            <th>Step</th>
            <th>Created</th>
            <th>Retry</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(jobs ?? []).map((j) => (
            <tr key={j.id}>
              {isAdmin && (
                <td>
                  <input
                    type="checkbox"
                    checked={checked.has(j.id)}
                    onChange={() => toggleCheck(j.id)}
                    aria-label={`Select job ${j.id}`}
                  />
                </td>
              )}
              <td>
                <a href="#" onClick={(e) => { e.preventDefault(); setSelected(j); }}>
                  {j.id}
                </a>
              </td>
              <td>{j.series}</td>
              <td>{j.episode}</td>
              <td className="muted">{j.script_type}</td>
              <td>{jobBadge(j.status)}</td>
              <td>
                {isAdmin && j.status === 'pending' && (flows ?? []).length > 0 ? (
                  <select
                    value={j.flow_id}
                    onChange={(e) => changeFlow(j.id, Number(e.target.value))}
                    title="Change the flow before this job starts"
                  >
                    {(flows ?? []).map((f) => (
                      <option key={f.id} value={f.id}>
                        {f.name}
                        {f.is_default ? ' (default)' : ''}
                      </option>
                    ))}
                  </select>
                ) : (
                  <span className="muted">{j.flow_id ? flowName(j.flow_id) : '—'}</span>
                )}
              </td>
              <td>
                {isAdmin && j.status === 'pending' ? (
                  <select
                    value={j.priority ?? 0}
                    onChange={(e) => changePriority(j.id, Number(e.target.value))}
                    title="Set job priority"
                  >
                    <option value={0}>Normal</option>
                    <option value={1}>High</option>
                  </select>
                ) : j.priority === 1 ? (
                  <span className="badge yellow">High</span>
                ) : null}
              </td>
              <td className="muted">{j.node_id ? nodeName(j.node_id) : '—'}</td>
              <td className="muted">{j.step || '—'}</td>
              <td className="muted">{fmtTime(j.created_at)}</td>
              <td className="muted">
                {(j.retry_count ?? 0) > 0 && (
                  <>
                    <span>retry {j.retry_count}</span>
                    {j.next_retry_at &&
                      (parseTime(j.next_retry_at)?.getTime() ?? 0) > Date.now() && (
                      <span className="muted" style={{ marginLeft: 4 }}>
                        waits until {fmtTime(j.next_retry_at)}
                      </span>
                    )}
                  </>
                )}
              </td>
              <td>
                {isAdmin && j.status === 'pending' && (
                  <button className="btn" onClick={() => cancel(j.id)}>
                    Cancel
                  </button>
                )}
                {isAdmin && ['failed', 'cancelled'].includes(j.status) && (
                  <button className="btn" onClick={() => retry(j.id)}>
                    Retry
                  </button>
                )}
                <button
                  className="btn"
                  onClick={() => setLogJob(j)}
                  disabled={!TERMINAL.has(j.status)}
                  title={TERMINAL.has(j.status) ? 'View full log' : 'No log until the job finishes'}
                >
                  Log
                </button>
                {isAdmin && TERMINAL.has(j.status) && (
                  <button
                    className="btn danger"
                    aria-label={`Delete job ${j.id}`}
                    onClick={() => { setDeleteConfirm(j); setDeleteMsg(''); }}
                    title="Delete this terminal job from the history"
                  >
                    Delete
                  </button>
                )}
              </td>
            </tr>
          ))}
          {(jobs ?? []).length === 0 && (
            <tr>
              <td colSpan={13} className="muted">
                No jobs {filter ? `with status ${filter}` : 'yet'}.
              </td>
            </tr>
          )}
        </tbody>
      </table>

      {selected && (
        <div className="card" style={{ marginTop: 16 }}>
          <h3 style={{ marginTop: 0 }}>
            Job #{selected.id} — {selected.series} Ep {selected.episode}
          </h3>
          <p className="muted">
            dir: {selected.episode_dir} · flow: {selected.flow_id ? flowName(selected.flow_id) : '—'} ·
            exit code: {selected.exit_code}
          </p>
          {selected.error && <div className="error-box">{selected.error}</div>}
          {selected.step_timings && selected.step_timings.length > 0 && (
            <StepTimingsView timings={selected.step_timings} />
          )}
          {selected.log_tail && (
            <>
              <h4>Log tail</h4>
              <pre>{selected.log_tail}</pre>
            </>
          )}
          <button className="btn" onClick={() => setSelected(null)}>
            Close
          </button>
        </div>
      )}

      {logJob && (
        <JobLogDialog
          jobId={logJob.id}
          jobLabel={`${logJob.series} Ep ${logJob.episode}`}
          stepTimings={logJob.step_timings}
          jobStatus={logJob.status}
          metrics={logJob.metrics}
          onClose={() => setLogJob(null)}
        />
      )}

      {/* Delete confirm dialog: shows the job id + series/episode and the
          409 error inline if the job is no longer terminal. */}
      {deleteConfirm && (
        <div className="modal-backdrop" onClick={() => !deleteMsg && setDeleteConfirm(null)}>
          <div className="modal card" style={{ maxWidth: 400 }} onClick={(e) => e.stopPropagation()}>
            <h3 style={{ marginTop: 0 }}>Delete job #{deleteConfirm.id}?</h3>
            <p className="muted" style={{ fontSize: 12 }}>
              {deleteConfirm.series} Ep {deleteConfirm.episode} · status: {deleteConfirm.status}
            </p>
            <p className="muted" style={{ fontSize: 12 }}>
              Removes the job from the history. Only terminal jobs (done/failed/
              cancelled) can be deleted.
            </p>
            {deleteMsg && <div className="error-box">{deleteMsg}</div>}
            <div className="toolbar" style={{ marginBottom: 0 }}>
              <button className="btn danger" onClick={() => deleteJobConfirm(deleteConfirm)}>
                Confirm delete
              </button>
              <button onClick={() => { setDeleteConfirm(null); setDeleteMsg(''); }}>
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}
