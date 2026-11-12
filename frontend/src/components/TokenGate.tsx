import { useEffect, useState } from 'react';
import { api, setToken, setCurrentUser } from '../api/client';

// LoginGate collects a username/password and exchanges them for a session
// via POST /api/auth/login. Replaces the old static admin-token prompt.
// When the controller has OIDC enabled (health.oidc_enabled), an additional
// "Sign in with SSO" button starts the backend authorization-code flow.
export default function TokenGate({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [ssoEnabled, setSsoEnabled] = useState(false);

  // The SSO button is env-gated server-side: health is the unauthenticated
  // bootstrap call, and oidc_enabled is false/absent on local-only installs
  // (the default) — the button then never renders. A failed health probe
  // simply leaves the button hidden; local login still works.
  useEffect(() => {
    let alive = true;
    api
      .health()
      .then((h) => {
        if (alive) setSsoEnabled(h.oidc_enabled === true);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);

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
    <div className="login-wrap">
      <div className="login-card">
        <div className="brand">
          <span className="mark" aria-hidden="true">E</span>
          <h1>
            encode-system
            <small>farm control</small>
          </h1>
        </div>
        <h2>Sign in</h2>
        <p className="muted" style={{ fontSize: 13, marginBottom: 20 }}>
          Management-plane login. The admin account is created on the
          controller&apos;s first startup (see ENCODE_ADMIN_USER /
          ENCODE_ADMIN_PASSWORD).
        </p>
        {error && <div className="error-box">{error}</div>}
        <div className="field">
          <label htmlFor="login-user">Username</label>
          <input
            id="login-user"
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </div>
        <div className="field">
          <label htmlFor="login-pass">Password</label>
          <input
            id="login-pass"
            type="password"
            autoComplete="current-password"
            placeholder="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && submit()}
          />
        </div>
        <button className="btn primary" onClick={submit} disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
        {ssoEnabled && (
          <button
            className="btn"
            onClick={() => {
              // Full-page navigation: the backend generates state/nonce/PKCE
              // and redirects to the identity provider. The callback lands
              // back on / with the session token in the URL fragment.
              window.location.assign('/api/auth/oidc/login');
            }}
          >
            Sign in with SSO
          </button>
        )}
      </div>
    </div>
  );
}
