import { useState } from 'react';
import { setToken } from '../api/client';

// TokenGate prompts once for the controller admin token and stores it locally.
export default function TokenGate({ onDone }: { onDone: () => void }) {
  const [value, setValue] = useState('');
  const [error, setError] = useState('');

  function submit() {
    const t = value.trim();
    if (!t) {
      setError('Token is required.');
      return;
    }
    setToken(t);
    onDone();
  }

  return (
    <div className="main" style={{ maxWidth: 420, margin: '10vh auto' }}>
      <div className="card">
        <h2>Controller access</h2>
        <p className="muted">
          Enter the admin token configured on the controller (ENCODE_ADMIN_TOKEN).
        </p>
        {error && <div className="error-box">{error}</div>}
        <input
          style={{ width: '100%', marginBottom: 12 }}
          type="password"
          placeholder="admin token"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submit()}
        />
        <button className="btn primary" onClick={submit}>
          Connect
        </button>
      </div>
    </div>
  );
}
