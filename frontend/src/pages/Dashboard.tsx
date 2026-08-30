import { api } from '../api/client';
import type { Job, Node, Settings } from '../types';
import { usePolling } from '../hooks/usePolling';
import { jobBadge, nodeBadge, timeAgo } from '../components/helpers';

// Dashboard gives the at-a-glance farm view: node health + active queue.
export default function Dashboard() {
  const { data: nodes, error: nodesErr } = usePolling<Node[]>(() => api.nodes(), 4000);
  const { data: jobs } = usePolling<Job[]>(() => api.jobs(), 4000);
  const { data: settings } = usePolling<Settings>(() => api.settings(), 60000);

  const active = (jobs ?? []).filter((j) => ['assigned', 'running'].includes(j.status));
  const pending = (jobs ?? []).filter((j) => j.status === 'pending');
  const online = (nodes ?? []).filter((n) => n.online);
  const busy = (nodes ?? []).filter((n) => n.status === 'busy');
  const idle = (nodes ?? []).filter((n) => n.status === 'idle');

  // Fleet metric averages are computed ONLY over nodes that reported
  // last_metrics (old agents and never-reported nodes are excluded entirely),
  // and GPU util additionally excludes hosts with gpu_util === -1 (no GPU).
  const withMetrics = (nodes ?? []).filter((n) => n.last_metrics);
  const gpuNodes = withMetrics.filter((n) => (n.last_metrics!.gpu_util ?? -1) >= 0);
  const avg = (sel: Node[], pick: (m: NonNullable<Node['last_metrics']>) => number) =>
    sel.length === 0
      ? null
      : sel.reduce((s, n) => s + pick(n.last_metrics!), 0) / sel.length;
  const avgFps = avg(withMetrics, (m) => m.encode_fps);
  const avgGpu = avg(gpuNodes, (m) => m.gpu_util);

  return (
    <>
      <h2>Dashboard</h2>
      {nodesErr && <div className="error-box">{nodesErr}</div>}

      {/* Fleet summary strip: busy/idle/online counts + fleet avg encode
          fps and avg GPU util (computed only from nodes reporting
          last_metrics; GPU util further excludes no-GPU hosts). */}
      <div className="card" style={{ display: 'flex', gap: 24, flexWrap: 'wrap', alignItems: 'center' }}>
        <Stat label="Busy" value={busy.length} tone="blue" />
        <Stat label="Idle" value={idle.length} tone="green" />
        <Stat label="Online" value={`${online.length}/${nodes?.length ?? 0}`} />
        <Stat
          label="Avg encode fps"
          value={avgFps !== null ? avgFps.toFixed(1) : '—'}
          title="Average encode fps across nodes reporting metrics"
        />
        <Stat
          label="Avg GPU util"
          value={avgGpu !== null ? `${Math.round(avgGpu)}%` : '—'}
          title="Average GPU utilization across GPU-equipped nodes reporting metrics"
        />
        {withMetrics.length === 0 && (
          <span className="muted" style={{ fontSize: 12 }}>no metrics yet</span>
        )}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Farm</h3>
        <p className="muted">
          {online.length}/{nodes?.length ?? 0} nodes online · {pending.length} pending ·{' '}
          {active.length} encoding
          {settings && (
            <>
              {' '}· group [{settings.group}] · tag [{settings.tag}] · reboot after{' '}
              {settings.tasks_before_reboot} tasks
            </>
          )}
        </p>
        <table>
          <thead>
            <tr>
              <th>Node</th>
              <th>Status</th>
              <th>Tasks this boot</th>
              <th>Agent</th>
              <th>Last seen</th>
            </tr>
          </thead>
          <tbody>
            {(nodes ?? []).map((n) => (
              <tr key={n.id}>
                <td>{n.name}</td>
                <td>{nodeBadge(n.status, !!n.online)}</td>
                <td>
                  {n.tasks_since_boot}
                  {settings && n.tasks_since_boot >= settings.tasks_before_reboot ? ' ⚠ reboot' : ''}
                </td>
                <td className="muted">{n.agent_version || '—'}</td>
                <td className="muted">{timeAgo(n.last_seen)}</td>
              </tr>
            ))}
            {(nodes ?? []).length === 0 && (
              <tr>
                <td colSpan={5} className="muted">
                  No nodes registered yet — add one on the Nodes page.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Active jobs</h3>
        <table>
          <thead>
            <tr>
              <th>Job</th>
              <th>Episode</th>
              <th>Status</th>
              <th>Step</th>
              <th>Progress</th>
            </tr>
          </thead>
          <tbody>
            {active.map((j) => (
              <tr key={j.id}>
                <td>#{j.id}</td>
                <td>{j.series} — {j.episode}</td>
                <td>{jobBadge(j.status)}</td>
                <td>{j.step || '—'}</td>
                <td>
                  <div className="progress-track">
                    <div className="progress-fill" style={{ width: `${j.progress ?? 0}%` }} />
                  </div>
                </td>
              </tr>
            ))}
            {active.length === 0 && (
              <tr>
                <td colSpan={5} className="muted">
                  Nothing encoding right now.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}

// Stat is a single label/value card in the fleet summary strip. `tone`
// optional color mirrors the badge palette (blue=busy, green=idle) so the
// strip reads at a glance without a new stylesheet.
function Stat({
  label,
  value,
  tone,
  title,
}: {
  label: string;
  value: string | number;
  tone?: 'blue' | 'green' | 'gray';
  title?: string;
}) {
  const toneCls = tone ?? 'gray';
  return (
    <div title={title} style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <span className="muted" style={{ fontSize: 11, textTransform: 'uppercase' }}>{label}</span>
      <span className={`badge ${toneCls}`} style={{ fontSize: 16, alignSelf: 'flex-start' }}>
        {value}
      </span>
    </div>
  );
}
