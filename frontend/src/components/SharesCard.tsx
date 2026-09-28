import { useCallback, useEffect, useState } from 'react';
import type { Share, ShareKind, ShareRole } from '../types';
import { api } from '../api/client';

// emptyForm is the blank editor state for a new share.
const emptyForm = {
  name: '',
  kind: 'smb' as ShareKind,
  role: 'scripts' as ShareRole,
  server: '',
  path: '',
  port: 0,
  username: '',
  password: '',
  endpoint: '',
  region: '',
  use_tls: false,
  enabled: true,
  mount_path: '',
};

type FormState = typeof emptyForm;

// SharesCard manages storage sources: one enabled share per pipeline role
// (scripts/release) wins dispatch/provisioning preference smb > nfs > s3.
// Passwords are write-only — the API returns has_password, never the value,
// so editing leaves the stored credential unless a new one is typed.
export default function SharesCard() {
  const [shares, setShares] = useState<Share[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState<FormState | null>(null); // null = closed
  const [editId, setEditId] = useState<number | null>(null);

  const load = useCallback(() => {
    api
      .shares()
      .then((r) => setShares(r ?? []))
      .catch((e) => setError(String(e)));
  }, []);

  useEffect(load, [load]);

  function startCreate() {
    setEditId(null);
    setForm({ ...emptyForm });
    setError('');
  }

  function startEdit(sh: Share) {
    setEditId(sh.id);
    setForm({
      name: sh.name,
      kind: sh.kind,
      role: sh.role,
      server: sh.server ?? '',
      path: sh.path ?? '',
      port: sh.port ?? 0,
      username: sh.username ?? '',
      password: '', // never prefilled: has_password means "keep unless replaced"
      endpoint: sh.endpoint ?? '',
      region: sh.region ?? '',
      use_tls: !!sh.use_tls,
      enabled: sh.enabled,
      mount_path: sh.mount_path ?? '',
    });
    setError('');
  }

  async function save() {
    if (!form) return;
    setBusy(true);
    setError('');
    // Build the request: drop the password field entirely when empty so the
    // backend keeps the stored credential (explicit "" would also keep it,
    // but omitting is unambiguous).
    const body: Partial<Share> & { password?: string } = {
      name: form.name.trim(),
      kind: form.kind,
      role: form.role,
      server: form.server.trim(),
      path: form.path.trim(),
      port: form.port || 0,
      username: form.username.trim(),
      endpoint: form.endpoint.trim(),
      region: form.region.trim(),
      use_tls: form.use_tls,
      enabled: form.enabled,
      mount_path: form.mount_path.trim(),
    };
    if (form.password) body.password = form.password;
    try {
      if (editId == null) {
        await api.createShare(body);
      } else {
        await api.updateShare(editId, body);
      }
      setForm(null);
      load();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove(sh: Share) {
    setBusy(true);
    setError('');
    try {
      await api.deleteShare(sh.id);
      load();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function toggle(sh: Share) {
    setBusy(true);
    setError('');
    try {
      await api.updateShare(sh.id, { ...sh, enabled: !sh.enabled } as Partial<Share>);
      load();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) =>
    setForm((f) => (f ? { ...f, [k]: v } : f));

  // Per-kind field hints keep the form honest without a wall of docs.
  const pathLabel =
    form?.kind === 's3' ? 'Bucket' : form?.kind === 'smb' ? 'Share name' : 'Export path';
  const serverLabel = form?.kind === 's3' ? 'Server (if no endpoint)' : 'Server';

  return (
    <div className="card">
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <h3 style={{ marginTop: 0 }}>Storage shares</h3>
        <button className="btn primary" onClick={startCreate} aria-label="Add share">
          Add share
        </button>
      </div>
      <p className="muted">
        One enabled share per role wins (smb &gt; nfs &gt; s3 for mounting; s3 roles stage
        per job — the agent downloads sources and uploads outputs). SMB needs a user with
        read/write on the share; NFS needs the export already allowed for the nodes.
      </p>
      {error && <div className="error-box">{error}</div>}

      {shares.length === 0 && !form ? (
        <p className="muted">
          No shares configured — provisioning falls back to the legacy NFS settings fields.
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>Role</th>
              <th>Source</th>
              <th>Creds</th>
              <th>Enabled</th>
              <th />
              <th />
            </tr>
          </thead>
          <tbody>
            {shares.map((sh) => (
              <tr key={sh.id}>
                <td>{sh.name}</td>
                <td>
                  <code>{sh.kind}</code>
                </td>
                <td>{sh.role}</td>
                <td>
                  {sh.kind === 's3'
                    ? `${sh.endpoint || sh.server}/${sh.path}`
                    : `${sh.server}:${sh.path}`}
                </td>
                <td>{sh.has_password ? '••••' : '—'}</td>
                <td>
                  <input
                    type="checkbox"
                    aria-label={`Enable ${sh.name}`}
                    checked={sh.enabled}
                    disabled={busy}
                    onChange={() => toggle(sh)}
                  />
                </td>
                <td>
                  <button aria-label={`Edit ${sh.name}`} onClick={() => startEdit(sh)}>
                    Edit
                  </button>
                </td>
                <td>
                  <button aria-label={`Delete ${sh.name}`} disabled={busy} onClick={() => remove(sh)}>
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {form && (
        <div style={{ marginTop: 12, borderTop: '1px solid var(--border)', paddingTop: 12 }}>
          <h4>{editId == null ? 'New share' : `Edit ${form.name || 'share'}`}</h4>
          <div className="toolbar" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <label>
              <span style={{ display: 'block', marginBottom: 2 }}>Name</span>
              <input
                aria-label="Share name"
                value={form.name}
                onChange={(e) => set('name', e.target.value)}
                style={{ width: 160 }}
              />
            </label>
            <label>
              <span style={{ display: 'block', marginBottom: 2 }}>Kind</span>
              <select
                aria-label="Share kind"
                value={form.kind}
                onChange={(e) => set('kind', e.target.value as ShareKind)}
              >
                <option value="smb">smb</option>
                <option value="nfs">nfs</option>
                <option value="s3">s3</option>
              </select>
            </label>
            <label>
              <span style={{ display: 'block', marginBottom: 2 }}>Role</span>
              <select
                aria-label="Share role"
                value={form.role}
                onChange={(e) => set('role', e.target.value as ShareRole)}
              >
                <option value="scripts">scripts</option>
                <option value="release">release</option>
              </select>
            </label>
            {form.kind !== 's3' && (
              <label>
                <span style={{ display: 'block', marginBottom: 2 }}>{serverLabel}</span>
                <input
                  aria-label="Share server"
                  value={form.server}
                  onChange={(e) => set('server', e.target.value)}
                  style={{ width: 140 }}
                />
              </label>
            )}
            {form.kind === 's3' && (
              <>
                <label>
                  <span style={{ display: 'block', marginBottom: 2 }}>Endpoint</span>
                  <input
                    aria-label="S3 endpoint"
                    value={form.endpoint}
                    placeholder="minio:9000"
                    onChange={(e) => set('endpoint', e.target.value)}
                    style={{ width: 180 }}
                  />
                </label>
                <label>
                  <span style={{ display: 'block', marginBottom: 2 }}>Region</span>
                  <input
                    aria-label="S3 region"
                    value={form.region}
                    onChange={(e) => set('region', e.target.value)}
                    style={{ width: 100 }}
                  />
                </label>
                <label style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <input
                    type="checkbox"
                    aria-label="Use TLS"
                    checked={form.use_tls}
                    onChange={(e) => set('use_tls', e.target.checked)}
                  />
                  TLS
                </label>
              </>
            )}
            <label>
              <span style={{ display: 'block', marginBottom: 2 }}>{pathLabel}</span>
              <input
                aria-label="Share path"
                value={form.path}
                onChange={(e) => set('path', e.target.value)}
                style={{ width: 180 }}
              />
            </label>
            {form.kind === 'smb' && (
              <>
                <label>
                  <span style={{ display: 'block', marginBottom: 2 }}>User</span>
                  <input
                    aria-label="Share username"
                    value={form.username}
                    onChange={(e) => set('username', e.target.value)}
                    style={{ width: 120 }}
                  />
                </label>
                <label>
                  <span style={{ display: 'block', marginBottom: 2 }}>
                    Password{editId != null ? ' (blank = keep)' : ''}
                  </span>
                  <input
                    type="password"
                    aria-label="Share password"
                    value={form.password}
                    onChange={(e) => set('password', e.target.value)}
                    style={{ width: 140 }}
                  />
                </label>
              </>
            )}
            {form.kind === 's3' && (
              <>
                <label>
                  <span style={{ display: 'block', marginBottom: 2 }}>Access key</span>
                  <input
                    aria-label="S3 access key"
                    value={form.username}
                    onChange={(e) => set('username', e.target.value)}
                    style={{ width: 140 }}
                  />
                </label>
                <label>
                  <span style={{ display: 'block', marginBottom: 2 }}>
                    Secret key{editId != null ? ' (blank = keep)' : ''}
                  </span>
                  <input
                    type="password"
                    aria-label="S3 secret key"
                    value={form.password}
                    onChange={(e) => set('password', e.target.value)}
                    style={{ width: 140 }}
                  />
                </label>
              </>
            )}
          </div>
          <div className="toolbar" style={{ marginTop: 8 }}>
            <button className="btn primary" disabled={busy || !form.name.trim()} onClick={save}>
              {editId == null ? 'Create' : 'Save'}
            </button>
            <button disabled={busy} onClick={() => setForm(null)}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
