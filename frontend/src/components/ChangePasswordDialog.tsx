import { useState } from 'react';
import { api } from '../api/client';

// ChangePasswordDialog lets the logged-in admin rotate their password from
// the UI, removing the need to keep it in the controller's environment file.
// The server verifies the current password, enforces a minimum length, and
// revokes all other sessions on success.
export default function ChangePasswordDialog({ onClose }: { onClose: () => void }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState('');
  const [success, setSuccess] = useState(false);
  const [busy, setBusy] = useState(false);

  async function submit() {
    if (busy || success) return; // Enter key-repeat / double-click guard
    setError('');
    if (!current || !next) {
      setError('All fields are required.');
      return;
    }
    if (next !== confirm) {
      setError('New passwords do not match.');
      return;
    }
    if (next.length < 10) {
      setError('New password must be at least 10 characters.');
      return;
    }
    if (next === current) {
      setError('New password must differ from the current one.');
      return;
    }
    setBusy(true);
    try {
      await api.changePassword(current, next);
      setSuccess(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Password change failed');
    } finally {
      setBusy(false);
    }
  }

  if (success) {
    return (
      <div className="modal-backdrop" onClick={onClose} role="dialog" aria-modal="true">
        <div className="card modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 420 }}>
          <h3>Password changed</h3>
          <p className="muted">
            Your password was updated and every other active session was signed
            out. You can now remove <code>ENCODE_ADMIN_PASSWORD</code> from the
            controller&apos;s <code>.env</code> — the account hash in the
            database is the only source of truth. (If you ever lose this
            password, the recovery hatch is <code>ENCODE_ADMIN_FORCE_PASSWORD=1</code>{' '}
            with a temporary <code>ENCODE_ADMIN_PASSWORD</code> — see the
            deployment docs.)
          </p>
          <div className="toolbar">
            <button className="btn primary" onClick={onClose}>
              Close
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="modal-backdrop" onClick={() => !busy && onClose()} role="dialog" aria-modal="true">
      <div className="card modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 420 }}>
        <h3>Change password</h3>
        <p className="muted">
          Requires your current password. All other active sessions are signed
          out on success.
        </p>
        {error && <div className="error-box">{error}</div>}
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>Current password</span>
          <input
            type="password"
            autoComplete="current-password"
            style={{ width: '100%' }}
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </label>
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>New password (min 10 characters)</span>
          <input
            type="password"
            autoComplete="new-password"
            style={{ width: '100%' }}
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
        </label>
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>Confirm new password</span>
          <input
            type="password"
            autoComplete="new-password"
            style={{ width: '100%' }}
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && !e.nativeEvent.isComposing && submit()}
          />
        </label>
        <div className="toolbar">
          <button className="btn primary" disabled={busy} onClick={submit}>
            {busy ? 'Changing…' : 'Change password'}
          </button>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}
