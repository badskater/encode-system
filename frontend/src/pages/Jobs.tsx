import { useState } from 'react';
import { api } from '../api/client';
import type { Flow, Job, JobStatus, Node } from '../types';
import { usePolling } from '../hooks/usePolling';
import { jobBadge, fmtTime } from '../components/helpers';
import JobLogDialog from '../components/JobLogDialog';
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

  const { data: jobs } = usePolling<Job[]>(
    () => api.jobs(filter || undefined),
    4000,
  );
  const { data: nodes } = usePolling<Node[]>(() => api.nodes(), 30000);
  const { data: flows } = usePolling<Flow[]>(() => api.flows(), 30000);

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

  return (
    <>
      <h2>Jobs</h2>
      {error && <div className="error-box">{error}</div>}

      <div className="toolbar">
        <label className="muted">Filter:</label>
        <select value={filter} onChange={(e) => setFilter(e.target.value as JobStatus | '')}>
          {FILTERS.map((f) => (
            <option key={f} value={f}>
              {f || 'all'}
            </option>
          ))}
        </select>
      </div>

      <table>
        <thead>
          <tr>
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
                {j.status === 'pending' && (flows ?? []).length > 0 ? (
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
                {j.status === 'pending' ? (
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
                    {j.next_retry_at && new Date(j.next_retry_at).getTime() > Date.now() && (
                      <span className="muted" style={{ marginLeft: 4 }}>
                        waits until {fmtTime(j.next_retry_at)}
                      </span>
                    )}
                  </>
                )}
              </td>
              <td>
                {j.status === 'pending' && (
                  <button className="btn" onClick={() => cancel(j.id)}>
                    Cancel
                  </button>
                )}
                {['failed', 'cancelled'].includes(j.status) && (
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
              </td>
            </tr>
          ))}
          {(jobs ?? []).length === 0 && (
            <tr>
              <td colSpan={12} className="muted">
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
          onClose={() => setLogJob(null)}
        />
      )}
    </>
  );
}
