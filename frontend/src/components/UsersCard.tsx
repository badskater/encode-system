import { useCallback, useEffect, useState } from 'react';
import type { User } from '../types';
import { api } from '../api/client';
import { fmtTime } from './helpers';

// UsersCard manages management-plane accounts: list, create (with role),
// delete (self/last-admin guarded server-side), and reset password. Rendered
// only on the Settings page when the logged-in user is an admin (the card is
// conditionally mounted by Settings based on useIsAdmin). The backend
// enforces all guards — self-delete and last-admin delete return 409.
export default function UsersCard() {
  const [users, setUsers] = useState<User[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<'admin' | 'viewer'>('viewer');
  // confirmDelete holds the user id awaiting the operator's confirm click.
  // null = no delete dialog open. The 409 self/last-admin error surfaces
  // inline in the dialog.
  const [confirmDelete, setConfirmDelete] = useState<User | null>(null);
  const [deleteMsg, setDeleteMsg] = useState('');
  // resetTarget holds the user whose password reset form is open.
  const [resetTarget, setResetTarget] = useState<User | null>(null);
  const [resetPw, setResetPw] = useState('');
  const [resetMsg, setResetMsg] = useState('');

  const load = useCallback(() => {
    api.listUsers().then(setUsers).catch((err) => setError(String(err)));
  }, []);

  useEffect(load, [load]);

  async function create() {
    setBusy(true);
    setError('');
    try {
      await api.createUser(name.trim(), password, role);
      setName('');
      setPassword('');
      setRole('viewer');
      load();
    } catch (err) {
      setError(String(err instanceof Error ? err.message.replace(/^\d+:\s*/, '') : err));
    } finally {
      setBusy(false);
    }
  }

  async function remove(user: User) {
    setBusy(true);
    setDeleteMsg('');
    try {
      await api.deleteUser(user.id);
      setConfirmDelete(null);
      load();
    } catch (err) {
      setDeleteMsg(String(err instanceof Error ? err.message.replace(/^\d+:\s*/, '') : err));
    } finally {
      setBusy(false);
    }
  }

  async function resetPassword() {
    if (!resetTarget) return;
    setBusy(true);
    setResetMsg('');
    try {
      await api.resetUserPassword(resetTarget.id, resetPw);
      setResetTarget(null);
      setResetPw('');
      setResetMsg('');
    } catch (err) {
      setResetMsg(String(err instanceof Error ? err.message.replace(/^\d+:\s*/, '') : err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h3 style={{ marginTop: 0 }}>Users</h3>
      <p className="muted">
        Management-plane accounts. <code>admin</code> has full management
        access; <code>viewer</code> is read-only (tables and dashboards, no
        mutations). Deleting your own account or the last admin is refused
        server-side.
      </p>
      {error && <div className="error-box">{error}</div>}

      {/* Create form */}
      <div className="toolbar" style={{ alignItems: 'flex-end', margin: '12px 0' }}>
        <label style={{ flex: 1 }}>
          <span style={{ display: 'block', marginBottom: 2 }}>Username</span>
          <input
            aria-label="New user username"
            value={name}
            placeholder="e.g. ops-viewer"
            onChange={(e) => setName(e.target.value)}
            maxLength={64}
          />
        </label>
        <label style={{ flex: 1 }}>
          <span style={{ display: 'block', marginBottom: 2 }}>Password</span>
          <input
            type="password"
            aria-label="New user password"
            value={password}
            placeholder="min 10 chars"
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <label>
          <span style={{ display: 'block', marginBottom: 2 }}>Role</span>
          <select
            aria-label="New user role"
            value={role}
            onChange={(e) => setRole(e.target.value as 'admin' | 'viewer')}
          >
            <option value="viewer">viewer</option>
            <option value="admin">admin</option>
          </select>
        </label>
        <button className="btn primary" disabled={busy || !name.trim() || !password} onClick={create}>
          Create user
        </button>
      </div>

      {/* Users table */}
      {users.length === 0 ? (
        <p className="muted">No users yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Username</th>
              <th>Role</th>
              <th>Created</th>
              <th />
              <th />
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id}>
                <td>{u.username}</td>
                <td>
                  {u.role === 'admin' ? (
                    <span className="badge" style={{ background: 'var(--accent-soft)', color: 'var(--accent-hi)', borderColor: 'rgba(157,107,255,0.2)' }}>
                      admin
                    </span>
                  ) : (
                    <span className="badge gray">viewer</span>
                  )}
                </td>
                <td className="muted">{u.created_at ? fmtTime(u.created_at) : '—'}</td>
                <td>
                  <button
                    aria-label={`Reset password for ${u.username}`}
                    disabled={busy}
                    onClick={() => { setResetTarget(u); setResetPw(''); setResetMsg(''); }}
                  >
                    Reset password
                  </button>
                </td>
                <td>
                  <button
                    aria-label={`Delete user ${u.username}`}
                    disabled={busy}
                    onClick={() => { setConfirmDelete(u); setDeleteMsg(''); }}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {/* Delete confirm dialog */}
      {confirmDelete && (
        <div className="card" style={{ borderColor: 'var(--yellow)', marginTop: 12 }}>
          <strong>Delete user “{confirmDelete.username}”?</strong>
          <p className="muted" style={{ fontSize: 12, margin: '6px 0' }}>
            The account is removed and all its sessions revoked. Deleting your
            own account or the last admin is refused.
          </p>
          {deleteMsg && <div className="error-box">{deleteMsg}</div>}
          <div className="toolbar" style={{ marginBottom: 0 }}>
            <button className="btn danger" disabled={busy} onClick={() => remove(confirmDelete)}>
              {busy ? 'Deleting…' : 'Confirm delete'}
            </button>
            <button disabled={busy} onClick={() => { setConfirmDelete(null); setDeleteMsg(''); }}>
              Cancel
            </button>
          </div>
        </div>
      )}

      {/* Password reset form */}
      {resetTarget && (
        <div className="card" style={{ borderColor: 'var(--accent)', marginTop: 12 }}>
          <strong>Reset password for “{resetTarget.username}”</strong>
          <p className="muted" style={{ fontSize: 12, margin: '6px 0' }}>
            Sets a new password (admin reset — no current password needed).
            All of the user's sessions are revoked.
          </p>
          {resetMsg && <div className="error-box">{resetMsg}</div>}
          <div className="toolbar" style={{ marginBottom: 0, alignItems: 'flex-end' }}>
            <label style={{ flex: 1 }}>
              <span style={{ display: 'block', marginBottom: 2 }}>New password</span>
              <input
                type="password"
                aria-label="Reset password new value"
                value={resetPw}
                placeholder="min 10 chars"
                onChange={(e) => setResetPw(e.target.value)}
                autoFocus
              />
            </label>
            <button className="btn primary" disabled={busy || !resetPw} onClick={resetPassword}>
              {busy ? 'Resetting…' : 'Reset'}
            </button>
            <button disabled={busy} onClick={() => { setResetTarget(null); setResetPw(''); setResetMsg(''); }}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
