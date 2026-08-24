import { useState } from 'react';
import { api, setToken, setCurrentUser } from '../api/client';

// LoginGate collects a username/password and exchanges them for a session
// via POST /api/auth/login. Replaces the old static admin-token prompt.
export default function TokenGate({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit() {
    if (!username.trim() || !password) {
      setError('Username and password are required.');
      return;
    }
    setBusy(true);
    setError('');
    try {
      const sess = await api.login(username.trim(), password);
      setToken(sess.token);
      setCurrentUser(sess.username);
      onDone();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'login failed');
      setBusy(false);
    }
  }

  return (
    <div className="main" style={{ maxWidth: 420, margin: '10vh auto' }}>
      <div className="card">
        <h2>Sign in</h2>
        <p className="muted">
          Management-plane login. The admin account is created on the
          controller&apos;s first startup (see ENCODE_ADMIN_USER /
          ENCODE_ADMIN_PASSWORD).
        </p>
        {error && <div className="error-box">{error}</div>}
        <label htmlFor="login-user" style={{ display: 'block', marginBottom: 4 }}>
          Username
        </label>
        <input
          id="login-user"
          style={{ width: '100%', marginBottom: 12 }}
          autoComplete="username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <label htmlFor="login-pass" style={{ display: 'block', marginBottom: 4 }}>
          Password
        </label>
        <input
          id="login-pass"
          style={{ width: '100%', marginBottom: 12 }}
          type="password"
          autoComplete="current-password"
          placeholder="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submit()}
        />
        <button className="btn primary" onClick={submit} disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </div>
    </div>
  );
}
