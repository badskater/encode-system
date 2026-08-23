import { useState } from 'react';
import { api } from '../api/client';
import type { Node } from '../types';
import { usePolling } from '../hooks/usePolling';
import { nodeBadge, timeAgo } from '../components/helpers';

// NodesPage manages the fleet: register nodes (showing the one-time token),
// enable/disable for work, and force reboots.
export default function NodesPage() {
  const { data: nodes, error } = usePolling<Node[]>(() => api.nodes(), 4000);
  const [newName, setNewName] = useState('');
  const [issued, setIssued] = useState<{ name: string; token: string } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  async function register() {
    const name = newName.trim();
    if (!name) return;
    try {
      const res = await api.createNode(name);
      setIssued({ name: res.node.name, token: res.token });
      setNewName('');
      setActionError(null);
    } catch (e) {
      setActionError(String(e));
    }
  }

  async function toggle(n: Node) {
    try {
      await api.setNodeEnabled(n.id, !n.enabled);
      setActionError(null);
    } catch (e) {
      setActionError(String(e));
    }
  }

  async function reboot(n: Node) {
    try {
      await api.rebootNode(n.id);
      setActionError(null);
    } catch (e) {
      setActionError(String(e));
    }
  }

  return (
    <>
      <h2>Nodes</h2>
      {error && <div className="error-box">{error}</div>}
      {actionError && <div className="error-box">{actionError}</div>}

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Register node</h3>
        <div className="toolbar">
          <input
            placeholder="node name (e.g. enc-01)"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && register()}
          />
          <button className="btn primary" onClick={register}>
            Register
          </button>
        </div>
        {issued && (
          <div className="card" style={{ background: '#0d1117' }}>
            <strong>{issued.name}</strong> registered. Copy this agent token now — it is shown
            only once:
            <pre>{issued.token}</pre>
            <button className="btn" onClick={() => setIssued(null)}>
              Dismiss
            </button>
          </div>
        )}
      </div>

      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Status</th>
            <th>Enabled</th>
            <th>Tasks</th>
            <th>Reboot pending</th>
            <th>Agent</th>
            <th>Lib</th>
            <th>Last seen</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(nodes ?? []).map((n) => (
            <tr key={n.id}>
              <td>{n.name}</td>
              <td>{nodeBadge(n.status, !!n.online)}</td>
              <td>
                <input type="checkbox" checked={n.enabled} onChange={() => toggle(n)} />
              </td>
              <td>{n.tasks_since_boot}</td>
              <td>{n.reboot_pending ? 'yes' : 'no'}</td>
              <td className="muted">{n.agent_version || '—'}</td>
              <td className="muted">{n.lib_version || '—'}</td>
              <td className="muted">{timeAgo(n.last_seen)}</td>
              <td>
                <button className="btn" onClick={() => reboot(n)}>
                  Reboot
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
