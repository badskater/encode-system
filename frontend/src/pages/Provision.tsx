import { useEffect, useRef, useState } from 'react';
import { api } from '../api/client';
import type { ProvisionRun, Settings } from '../types';
import { timeAgo } from '../components/helpers';

// ProvisionPage drives controller-side Ansible provisioning of fresh Windows
// nodes. The WinRM password is sent once per run and never persisted (server
// writes it to a temp vars file, deletes it on completion). Pairing is
// zero-touch: the controller issues a one-shot code per run.
export default function ProvisionPage() {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [runs, setRuns] = useState<ProvisionRun[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // Form state.
  const [host, setHost] = useState('');
  const [port, setPort] = useState('5985');
  const [scheme, setScheme] = useState('http');
  const [winrmUser, setWinrmUser] = useState('Administrator');
  const [password, setPassword] = useState('');
  const [nodeName, setNodeName] = useState('');
  const [installToolchain, setInstallToolchain] = useState(true);
  const [mountNFS, setMountNFS] = useState(true);
  const [pushBin, setPushBin] = useState(true);
  const [starting, setStarting] = useState(false);

  // Log viewer.
  const [openRunId, setOpenRunId] = useState<number | null>(null);
  const [openRun, setOpenRun] = useState<ProvisionRun | null>(null);
  const logRef = useRef<HTMLPreElement>(null);

  async function refresh() {
    try {
      setRuns(await api.provisionRuns());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  useEffect(() => {
    api.settings().then(setSettings).catch(() => setSettings(null));
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  // Track whether the user is scrolled to the bottom of the log; only
  // auto-follow when they are (so scrolling up to read earlier lines isn't
  // yanked back down on every poll).
  const stickToBottomRef = useRef(true);
  function onLogScroll() {
    const el = logRef.current;
    if (!el) return;
    stickToBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  }

  // Poll the open run's log while it is active. Stops once the run reaches a
  // terminal status (success/failed) instead of hammering the controller
  // forever while the log panel stays open.
  useEffect(() => {
    if (openRunId === null) return;
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | null = null;
    const poll = async () => {
      try {
        const run = await api.provisionRun(openRunId);
        if (cancelled) return;
        setOpenRun(run);
        if (stickToBottomRef.current) {
          requestAnimationFrame(() => {
            if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
          });
        }
        // Terminal: stop polling. (The run list still refreshes on its own
        // 5s interval, so the status badge stays current.)
        if (run.status === 'success' || run.status === 'failed') {
          if (timer) clearInterval(timer);
        }
      } catch {
        /* transient: keep polling */
      }
    };
    poll();
    timer = setInterval(poll, 2500);
    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
    };
  }, [openRunId]);

  async function start() {
    setError(null);
    setNotice(null);
    if (!host.trim() || !nodeName.trim() || !password) {
      setError('Host, node name, and WinRM password are required.');
      return;
    }
    if (!settings?.controller_url) {
      setError('Set the controller URL on the Settings page first — provisioned nodes dial that address.');
      return;
    }
    setStarting(true);
    try {
      const run = await api.startProvision({
        host: host.trim(),
        port: Number(port) || 5985,
        scheme,
        winrm_user: winrmUser.trim() || 'Administrator',
        winrm_password: password,
        node_name: nodeName.trim(),
        install_toolchain: installToolchain,
        mount_nfs: mountNFS,
        push_bin: pushBin,
      });
      setNotice(`Provisioning run #${run.id} started for ${run.host} — watch the log below.`);
      setPassword(''); // never hold the password in state after submit
      setOpenRunId(run.id);
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setStarting(false);
    }
  }

  const statusBadge = (s: ProvisionRun['status']) => {
    const colors: Record<ProvisionRun['status'], string> = {
      queued: '#8b949e',
      running: '#d29922',
      success: '#3fb950',
      failed: '#f85149',
    };
    return (
      <span style={{ color: colors[s], fontWeight: 600, textTransform: 'uppercase', fontSize: 12 }}>
        {s}
      </span>
    );
  };

  return (
    <>
      <h2>Provision node</h2>
      {error && <div className="error-box">{error}</div>}
      {notice && <div className="card" style={{ borderColor: '#2ea043' }}>{notice}</div>}

      <div className="card">
        <h3 style={{ marginTop: 0 }}>New Windows node</h3>
        <p className="muted">
          The controller runs Ansible against the node over WinRM: mounts the NFS
          shares from Settings, installs the toolchain (MediaInfo, AviSynth+,
          Python 64-bit, VapourSynth), pushes the tools folder, and registers the
          agent as a Windows service with a one-shot pairing code. The password
          is used for this run only and never stored.
        </p>
        {!settings?.controller_url && (
          <div className="error-box">
            No controller URL configured — open Settings and set it before provisioning.
          </div>
        )}
        <div className="toolbar">
          <label style={{ flex: 2 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Host (IP or DNS)</span>
            <input style={{ width: '100%' }} value={host} onChange={(e) => setHost(e.target.value)} placeholder="172.24.92.250" />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>WinRM port</span>
            <input style={{ width: '100%' }} value={port} onChange={(e) => setPort(e.target.value)} />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Scheme</span>
            <select style={{ width: '100%' }} value={scheme} onChange={(e) => setScheme(e.target.value)}>
              <option value="http">http</option>
              <option value="https">https</option>
            </select>
          </label>
        </div>
        <div className="toolbar">
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>WinRM user</span>
            <input style={{ width: '100%' }} value={winrmUser} onChange={(e) => setWinrmUser(e.target.value)} />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>WinRM password (used once, not stored)</span>
            <input style={{ width: '100%' }} type="password" autoComplete="off" value={password} onChange={(e) => setPassword(e.target.value)} />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Node name</span>
            <input style={{ width: '100%' }} value={nodeName} onChange={(e) => setNodeName(e.target.value)} placeholder="enc-03" />
          </label>
        </div>
        <div className="toolbar" style={{ gap: 16 }}>
          <label><input type="checkbox" checked={installToolchain} onChange={(e) => setInstallToolchain(e.target.checked)} /> Install toolchain (MediaInfo, AviSynth+, Python 64-bit, VapourSynth)</label>
          <label><input type="checkbox" checked={mountNFS} onChange={(e) => setMountNFS(e.target.checked)} /> Mount NFS shares</label>
          <label><input type="checkbox" checked={pushBin} onChange={(e) => setPushBin(e.target.checked)} /> Push tools folder (bin package)</label>
        </div>
        <button className="btn primary" disabled={starting} onClick={start}>
          {starting ? 'Starting…' : 'Provision node'}
        </button>
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Provisioning runs</h3>
        {runs.length === 0 && <p className="muted">No runs yet.</p>}
        {runs.length > 0 && (
          <table>
            <thead>
              <tr>
                <th>#</th>
                <th>Node</th>
                <th>Host</th>
                <th>Status</th>
                <th>Error</th>
                <th>Started</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr key={r.id}>
                  <td className="muted">{r.id}</td>
                  <td>{r.node_name}</td>
                  <td className="muted">{r.host}:{r.port}</td>
                  <td>{statusBadge(r.status)}</td>
                  <td className="muted" style={{ maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                    {r.error || '—'}
                  </td>
                  <td className="muted">{timeAgo(r.created_at)}</td>
                  <td>
                    <button className="btn" onClick={() => setOpenRunId(r.id)}>
                      Log
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {openRunId !== null && (
        <div className="card">
          <h3 style={{ marginTop: 0 }}>
            Run #{openRunId} log {openRun && <>— {statusBadge(openRun.status)}</>}
          </h3>
          <pre
            ref={logRef}
            onScroll={onLogScroll}
            style={{
              maxHeight: 420,
              overflow: 'auto',
              background: '#0d1117',
              padding: 12,
              borderRadius: 6,
              fontSize: 12,
              whiteSpace: 'pre-wrap',
            }}
          >
            {openRun?.log || (openRun?.status === 'queued' ? 'Queued — waiting for a free run slot…' : 'Loading…')}
          </pre>
          <button className="btn" onClick={() => { setOpenRunId(null); setOpenRun(null); }}>
            Close log
          </button>
        </div>
      )}
    </>
  );
}
