import { useCallback, useEffect, useState } from 'react';
import type { APIToken } from '../types';
import { api } from '../api/client';
import { timeAgo } from './helpers';

// APITokensCard manages scoped API tokens for external automation
// (Sonarr-style triggers, Grafana, scripts). Create shows the plaintext
// token exactly once with a copy button; the list shows metadata only.
export default function APITokensCard() {
  const [tokens, setTokens] = useState<APIToken[]>([]);
  const [error, setError] = useState('');
  const [name, setName] = useState('');
  const [scope, setScope] = useState<'admin' | 'read'>('read');
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState<{ name: string; token: string } | null>(null);

  const load = useCallback(() => {
    api.listTokens().then(setTokens).catch((err) => setError(String(err)));
  }, []);

  useEffect(load, [load]);

  async function create() {
    setBusy(true);
    setError('');
    try {
      const created = await api.createToken(name.trim(), scope);
      setIssued({ name: created.name, token: created.token });
      setName('');
      load();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function remove(id: number) {
    setBusy(true);
    setError('');
    try {
      await api.deleteToken(id);
      load();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h3 style={{ marginTop: 0 }}>API tokens</h3>
      <p className="muted">
        Scoped tokens for external automation — scripts, Grafana, Sonarr-style
        triggers. <code>read</code> tokens can only call GET endpoints;{' '}
        <code>admin</code> tokens get full management access. Send as{' '}
        <code>Authorization: Bearer &lt;token&gt;</code>. The plaintext token is
        shown exactly once at creation.
      </p>
      {error && <div className="error-box">{error}</div>}
      {issued && (
        <div className="card" style={{ borderColor: '#2ea043' }}>
          <strong>New token “{issued.name}” — copy it now, it won’t be shown again:</strong>
          <div style={{ display: 'flex', gap: 8, marginTop: 8, alignItems: 'center' }}>
            <code style={{ wordBreak: 'break-all', flex: 1 }} data-testid="issued-token">
              {issued.token}
            </code>
            <button
              aria-label="Copy token"
              onClick={() => navigator.clipboard?.writeText(issued.token)}
            >
              Copy
            </button>
            <button aria-label="Dismiss token" onClick={() => setIssued(null)}>
              Dismiss
            </button>
          </div>
        </div>
      )}

      <div className="toolbar" style={{ alignItems: 'flex-end', margin: '12px 0' }}>
        <label style={{ flex: 1 }}>
          <span style={{ display: 'block', marginBottom: 2 }}>Token name</span>
          <input
            aria-label="Token name"
            value={name}
            placeholder="e.g. grafana, sonarr"
            onChange={(e) => setName(e.target.value)}
            maxLength={64}
          />
        </label>
        <label>
          <span style={{ display: 'block', marginBottom: 2 }}>Scope</span>
          <select
            aria-label="Token scope"
            value={scope}
            onChange={(e) => setScope(e.target.value as 'admin' | 'read')}
          >
            <option value="read">read</option>
            <option value="admin">admin</option>
          </select>
        </label>
        <button className="btn primary" disabled={busy || !name.trim()} onClick={create}>
          Create token
        </button>
      </div>

      {tokens.length === 0 ? (
        <p className="muted">No tokens yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Scope</th>
              <th>Last used</th>
              <th>Created</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {tokens.map((tok) => (
              <tr key={tok.id}>
                <td>{tok.name}</td>
                <td>
                  <code>{tok.scope}</code>
                </td>
                <td>{tok.last_used_at ? timeAgo(tok.last_used_at) : <span className="muted">never</span>}</td>
                <td>{tok.created_at ? new Date(tok.created_at).toLocaleDateString() : ''}</td>
                <td>
                  <button aria-label={`Delete token ${tok.name}`} disabled={busy} onClick={() => remove(tok.id)}>
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
