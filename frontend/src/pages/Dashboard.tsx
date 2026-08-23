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

  return (
    <>
      <h2>Dashboard</h2>
      {nodesErr && <div className="error-box">{nodesErr}</div>}

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
