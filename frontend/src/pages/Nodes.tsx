import { useState } from 'react';
import { api } from '../api/client';
import type { Node, PairingCode } from '../types';
import { usePolling } from '../hooks/usePolling';
import { nodeBadge, timeAgo } from '../components/helpers';

// NodesPage manages the fleet: register nodes (showing the one-time token),
// enable/disable for work, and force reboots.
export default function NodesPage() {
  const { data: nodes, error } = usePolling<Node[]>(() => api.nodes(), 4000);
  const { data: codes } = usePolling<PairingCode[]>(() => api.pairingCodes(), 10000);
  const [newName, setNewName] = useState('');
  const [issued, setIssued] = useState<{ name: string; token: string } | null>(null);
  const [issuedPair, setIssuedPair] = useState<string | null>(null);
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

  async function issuePairingCode() {
    try {
      const res = await api.createPairingCode({ name_hint: newName.trim() || undefined, ttl_hours: 1 });
      setIssuedPair(res.code);
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

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Or: one-shot pairing code</h3>
        <p className="muted">
          Prefer zero-touch bootstrap? Issue a pairing code (valid 1 hour) and put it in the
          node's <code>agent.json</code> as <code>pairing_code</code> together with{' '}
          <code>node_name</code>. The agent registers itself on first start and stores its own
          credential — no manual token copy/paste.
        </p>
        <div className="toolbar">
          <button className="btn primary" onClick={issuePairingCode}>
            Issue pairing code
          </button>
        </div>
        {issuedPair && (
          <div className="card" style={{ background: '#0d1117' }}>
            Copy this pairing code now — it is shown only once and can be used exactly once:
            <pre>{issuedPair}</pre>
            <pre>{`{
  "controller_url": "https://controller:8080",
  "node_name": "<this node>",
  "pairing_code": "<paste code here>"
}`}</pre>
            <button className="btn" onClick={() => setIssuedPair(null)}>
              Dismiss
            </button>
          </div>
        )}
        {(codes ?? []).length > 0 && (
          <table>
            <thead>
              <tr>
                <th>Active codes</th>
                <th>Hint</th>
                <th>Expires</th>
              </tr>
            </thead>
            <tbody>
              {(codes ?? []).map((c) => (
                <tr key={c.id}>
                  <td className="muted">#{c.id}</td>
                  <td>{c.name_hint || '—'}</td>
                  <td className="muted">{timeAgo(c.expires_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
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
