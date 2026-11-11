import { useEffect, useRef, useState } from 'react';
import { api } from '../api/client';
import type { Settings, SettingsSave, UpdateCheck, UpdateManifest } from '../types';
import { timeAgo } from '../components/helpers';
import { useIsAdmin } from '../hooks/useRole';
import APITokensCard from '../components/APITokensCard';
import BackupsCard from '../components/BackupsCard';
import SharesCard from '../components/SharesCard';
import UsersCard from '../components/UsersCard';

// StringSettingKey = the Settings keys whose values are strings — the only
// keys the free-text field() helper may edit. Number fields go through their
// own inputs with explicit Number coercion, so a string can never be written
// into a number field by accident.
type StringSettingKey = {
  [K in keyof Settings]-?: Settings[K] extends string | undefined ? K : never;
}[keyof Settings];

// Publish payload caps — mirror the server-side limits so an oversized or
// mistyped file is rejected before a multi-GB upload even starts.
const MAX_AGENT_BYTES = 64 * 1024 * 1024; // 64 MiB
const MAX_LIB_BYTES = 4 * 1024 * 1024;    // 4 MiB
const MAX_BIN_BYTES = 1024 * 1024 * 1024; // 1 GiB

function checkPayload(file: File, maxBytes: number, kind: string): string | null {
  if (file.size > maxBytes) {
    return `${kind} is ${(file.size / 1048576).toFixed(1)} MiB — the limit is ${(maxBytes / 1048576).toFixed(0)} MiB`;
  }
  return null;
}

// clampInt coerces a possibly-NaN/out-of-range number input into [lo, hi].
function clampInt(v: number, lo: number, hi: number): number {
  if (!Number.isFinite(v)) return lo;
  return Math.min(hi, Math.max(lo, Math.round(v)));
}

// isHttpUrl mirrors the server's http(s)-with-host bound for the webhook and
// ntfy topic fields (new URL rejects scheme-less or host-less inputs).
function isHttpUrl(v: string): boolean {
  try {
    const u = new URL(v);
    return u.protocol === 'http:' || u.protocol === 'https:';
  } catch {
    return false;
  }
}

// isEmailAddr accepts a bare address or a "Name <addr>" display form — the
// same shapes net/mail.ParseAddress accepts server-side. The domain does NOT
// need a dot: ops@localhost is valid on the server (G6), so the client must
// not be stricter. (Quoted local parts remain a server-only edge case.)
function isEmailAddr(v: string): boolean {
  const m = v.match(/<([^>]+)>/);
  const addr = (m ? m[1] : v).trim();
  return /^[^\s@]+@[^\s@]+$/.test(addr);
}

// validateNotifyChannels mirrors the server's settings bounds for the three
// new channels so a typo is caught before the PUT. Returns an error message
// or null when valid. Empty fields = channel disabled (always valid).
function validateNotifyChannels(s: Settings): string | null {
  if (s.generic_webhook_url && !isHttpUrl(s.generic_webhook_url.trim())) {
    return 'Generic webhook URL must be an http(s) URL';
  }
  if (s.ntfy_topic_url && !isHttpUrl(s.ntfy_topic_url.trim())) {
    return 'ntfy topic URL must be an http(s) URL (e.g. https://ntfy.sh/mytopic)';
  }
  const port = s.smtp_port ?? 0;
  if (port !== 0 && (!Number.isInteger(port) || port < 1 || port > 65535)) {
    return 'SMTP port must be 0 (default 587) or 1-65535';
  }
  const host = (s.smtp_host ?? '').trim();
  const from = (s.smtp_from ?? '').trim();
  const to = (s.smtp_to ?? '').trim();
  if (host && (/\s/.test(host) || host.includes('://'))) {
    return 'SMTP host must be a hostname (no scheme, no spaces)';
  }
  if (host && (!from || !to)) {
    return 'SMTP from and to are required when a host is set';
  }
  if (!host && (from || to)) {
    return 'SMTP host is required when from/to are set';
  }
  if (from && !isEmailAddr(from)) {
    return 'SMTP from must be a valid email address';
  }
  if (to) {
    for (const addr of to.split(',')) {
      if (!isEmailAddr(addr.trim())) {
        return 'SMTP to must be comma-separated valid email addresses';
      }
    }
  }
  return null;
}

// SettingsPage edits the fleet's runtime configuration from the UI:
//  - NFS shares (documentation-grade: which exports, where they mount)
//  - Controller roots (scan + release dirs inside the container)
//  - Remote path mapping (the node-side dirs baked into rendered jobs)
//  - Behavior (scan interval, reboot threshold, release naming)
//  - Agent publishing: push a new agent binary, EncodeLib.ps1, or the
//    tools-folder (bin) zip to every node.
export default function SettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [manifest, setManifest] = useState<UpdateManifest | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // Publish form state.
  const [agentVersion, setAgentVersion] = useState('');
  const [agentFile, setAgentFile] = useState<File | null>(null);
  const [libVersion, setLibVersion] = useState('');
  const [libFile, setLibFile] = useState<File | null>(null);
  const [binVersion, setBinVersion] = useState('');
  const [binFile, setBinFile] = useState<File | null>(null);
  // bin-url form: fetch a package from a URL instead of uploading it
  // (e.g. a GitHub release asset on the encode-bin repo).
  const [binUrl, setBinUrl] = useState('');
  const [binUrlVersion, setBinUrlVersion] = useState('');
  const [binUrlSha, setBinUrlSha] = useState('');
  const [publishing, setPublishing] = useState('');
  // Agent publish platform picker: "windows" (default, .exe) or "linux".
  // Governs which slot the publish/rollback targets and which manifest rows
  // are highlighted. Windows and Linux slots are independent on the backend.
  const [platform, setPlatform] = useState<'windows' | 'linux'>('windows');
  // Update check state: null = not checked yet, result = last check, error
  // string = check failed (502 upstream). The check is best-effort and never
  // blocks other settings — it's a muted inline notice, not an error-box.
  const [updateCheck, setUpdateCheck] = useState<UpdateCheck | null>(null);
  const [updateError, setUpdateError] = useState('');
  const [checking, setChecking] = useState(false);
  // Write-only notification secrets: kept OUT of the settings object (the
  // API never returns them). Empty on load; a typed value is sent once on
  // save, then cleared — blank on the next save keeps the stored credential.
  const [ntfyToken, setNtfyToken] = useState('');
  const [smtpPassword, setSmtpPassword] = useState('');
  const isAdmin = useIsAdmin();
  // Refs to the file inputs: a successful publish clears the STATE but the
  // uncontrolled <input type=file> would still show the picked file name —
  // reset its value too so the UI matches reality.
  const agentFileRef = useRef<HTMLInputElement>(null);
  const libFileRef = useRef<HTMLInputElement>(null);
  const binFileRef = useRef<HTMLInputElement>(null);

  async function load() {
    // Settings and the manifest are INDEPENDENT resources: a manifest fetch
    // failure (e.g. nothing published yet) must not make the settings form
    // unusable.
    try {
      setSettings(await api.settings());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
    try {
      setManifest(await api.manifest());
    } catch {
      // Manifest unavailable: publishing UI degrades gracefully.
      setManifest(null);
    }
  }

  async function refreshManifest() {
    try {
      setManifest(await api.manifest());
    } catch {
      /* non-fatal */
    }
  }

  // checkForUpdate compares the running controller version to the latest
  // GitHub release tag. Best-effort — a 502 (upstream failure, rate-limit)
  // surfaces as a muted inline error, never an error-box.
  async function checkForUpdate() {
    setChecking(true);
    setUpdateError('');
    try {
      const res = await api.checkUpdate();
      // GitHub tags carry the leading "v" (tag_name = "v1.23.0") but the UI
      // renders its own "v" prefix — normalize here or the badge shows "vv1.23.0".
      setUpdateCheck({ ...res, latest: res.latest.replace(/^v/, '') });
    } catch (e) {
      setUpdateCheck(null);
      setUpdateError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setChecking(false);
    }
  }

  useEffect(() => {
    load();
  }, []);

  if (!settings) {
    return (
      <>
        <h2>Settings</h2>
        {error ? <div className="error-box">{error}</div> : <p className="muted">Loading…</p>}
      </>
    );
  }

  function set<K extends keyof Settings>(key: K, value: Settings[K]) {
    setSettings((s) => (s ? { ...s, [key]: value } : s));
  }

  // String-only variant for the free-text field() helper.
  function setString(key: StringSettingKey, value: string) {
    setSettings((s) => (s ? { ...s, [key]: value } : s));
  }

  async function save() {
    if (!settings) return;
    // Client-side bounds mirror the server validation for the notification
    // channels — surface the error before spending a round-trip.
    const invalid = validateNotifyChannels(settings);
    if (invalid) {
      setError(invalid);
      return;
    }
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      // Number inputs can yield NaN (cleared field) or bypass their min/max
      // attrs (Save is a button, not a form submit) — clamp server-bound
      // values to the documented ranges instead of persisting zeros.
      const toSave = {
        ...settings,
        scan_interval_seconds: clampInt(settings.scan_interval_seconds, 5, 3600),
        tasks_before_reboot: clampInt(settings.tasks_before_reboot, 1, 1000),
        // Retention allows 0 (keep forever) as a first-class value;
        // clampInt maps a cleared (NaN) field to lo=0 — the safe default.
        job_retention_days: clampInt(settings.job_retention_days ?? 0, 0, 3650),
        // 0 = disabled; a cleared (NaN) field maps to lo=0, the safe default.
        disk_alert_gb: clampInt(settings.disk_alert_gb ?? 0, 0, 100000),
        // Stall watchdog: 0 = off; same NaN→0 safe default, ceiling 1 week.
        stall_timeout_minutes: clampInt(settings.stall_timeout_minutes ?? 0, 0, 10080),
        // SMTP port: 0 = default 587; NaN (cleared) maps to 0.
        smtp_port: clampInt(settings.smtp_port ?? 0, 0, 65535),
      };
      // PUT payload: strip the GET-only has_* flags (the server tolerates
      // them, but they are output-only) and attach typed write-only secrets.
      // Omitted/empty secret = keep the stored value (shares pattern).
      const payload = { ...toSave } as SettingsSave & {
        has_ntfy_token?: boolean;
        has_smtp_password?: boolean;
      };
      delete payload.has_ntfy_token;
      delete payload.has_smtp_password;
      if (ntfyToken.trim()) payload.ntfy_token = ntfyToken.trim();
      if (smtpPassword) payload.smtp_password = smtpPassword;
      const saved = await api.saveSettings(payload);
      setSettings(saved);
      // Secrets were consumed: clear the inputs so the next save keeps them.
      setNtfyToken('');
      setSmtpPassword('');
      setNotice('Settings saved. Scanner and new jobs pick them up on the next cycle — no restart needed.');
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setSaving(false);
    }
  }

  async function publish(kind: 'agent' | 'lib' | 'bin' | 'bin-url') {
    setError(null);
    setNotice(null);
    setPublishing(kind);
    try {
      if (kind === 'agent') {
        if (!agentFile || !agentVersion.trim()) throw new Error('agent version + file are required');
        const tooBig = checkPayload(agentFile, MAX_AGENT_BYTES, 'agent binary');
        if (tooBig) throw new Error(tooBig);
        await api.publishAgent(agentVersion.trim(), agentFile, platform);
        setAgentFile(null);
        setAgentVersion('');
        if (agentFileRef.current) agentFileRef.current.value = '';
      } else if (kind === 'lib') {
        if (!libFile || !/^\d+$/.test(libVersion)) throw new Error('lib needs a numeric version + file');
        const tooBig = checkPayload(libFile, MAX_LIB_BYTES, 'EncodeLib');
        if (tooBig) throw new Error(tooBig);
        await api.publishLib(Number(libVersion), libFile);
        setLibFile(null);
        setLibVersion('');
        if (libFileRef.current) libFileRef.current.value = '';
      } else if (kind === 'bin') {
        if (!binFile || !/^\d+$/.test(binVersion)) throw new Error('bin needs a numeric version + file');
        const tooBig = checkPayload(binFile, MAX_BIN_BYTES, 'bin package');
        if (tooBig) throw new Error(tooBig);
        await api.publishBin(Number(binVersion), binFile);
        setBinFile(null);
        setBinVersion('');
        if (binFileRef.current) binFileRef.current.value = '';
      } else {
        // bin-url: the controller downloads the zip (e.g. a GitHub release
        // asset) and validates it exactly like a browser upload.
        if (!binUrl.trim() || !/^\d+$/.test(binUrlVersion)) {
          throw new Error('a download URL + numeric version are required');
        }
        if (!/^https?:\/\//.test(binUrl.trim())) {
          throw new Error('URL must start with http:// or https://');
        }
        await api.publishBinFromURL(binUrl.trim(), Number(binUrlVersion), binUrlSha.trim() || undefined);
        setBinUrl('');
        setBinUrlVersion('');
        setBinUrlSha('');
      }
      setNotice(`${kind === 'bin-url' ? 'bin package' : kind} published — idle nodes adopt it on their next heartbeat.`);
      // Refresh the manifest readout only — reloading settings here would
      // silently discard any unsaved edits in the form above.
      await refreshManifest();
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setPublishing('');
    }
  }

  // rollback promotes the previous agent release back to current. The
  // server keeps exactly one release of history (rotated on each publish);
  // with no rollback slot the endpoint 409s and the message surfaces in
  // the error box. Nodes self-downgrade on their next idle heartbeat.
  async function rollback() {
    setError(null);
    setNotice(null);
    setPublishing('rollback');
    try {
      const m = await api.rollbackAgent(platform);
      const ver = platform === 'linux' ? (m.linux_agent_version || '—') : m.agent_version;
      setNotice(`Rolled back to agent ${ver} — idle nodes adopt it on their next heartbeat.`);
      await refreshManifest();
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setPublishing('');
    }
  }

  const field = (label: string, hint: string, key: StringSettingKey, placeholder = '') => (
    <label style={{ display: 'block', marginBottom: 12 }}>
      <span style={{ display: 'block', marginBottom: 2 }}>{label}</span>
      <input
        style={{ width: '100%' }}
        value={String(settings[key] ?? '')}
        placeholder={placeholder}
        disabled={!isAdmin}
        onChange={(e) => setString(key, e.target.value)}
      />
      <span className="muted" style={{ fontSize: 12 }}>{hint}</span>
    </label>
  );

  // secretField renders a write-only credential input (SharesCard pattern):
  // never prefilled, password-type, and a "saved" placeholder when the API
  // reports the credential exists (has_* flag). Blank on save = keep it.
  const secretField = (
    label: string, hint: string, value: string, onChange: (v: string) => void,
    saved: boolean, placeholder: string,
  ) => (
    <label style={{ display: 'block', marginBottom: 12 }}>
      <span style={{ display: 'block', marginBottom: 2 }}>{label}</span>
      <input
        type="password"
        aria-label={label}
        autoComplete="new-password"
        style={{ width: '100%' }}
        value={value}
        placeholder={saved ? '•••• saved — leave blank to keep' : placeholder}
        disabled={!isAdmin}
        onChange={(e) => onChange(e.target.value)}
      />
      <span className="muted" style={{ fontSize: 12 }}>{hint}</span>
    </label>
  );

  return (
    <>
      <h2>Settings</h2>
      {error && <div className="error-box">{error}</div>}
      {notice && <div className="card" style={{ borderColor: '#2ea043' }}>{notice}</div>}

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Controller URL (as seen by nodes)</h3>
        <p className="muted">
          The address the Windows nodes use to reach this controller — the one
          baked into every provisioned <code>agent.json</code>. Use the Docker
          host's LAN IP (the container's own hostname is usually unreachable
          from outside). Required before provisioning a node.
        </p>
        {field('Controller URL', 'e.g. http://172.24.92.232:8080', 'controller_url', 'http://172.24.92.232:8080')}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>NFS shares</h3>
        <p className="muted">
          Record where the shares come from. Mounting itself stays in compose
          (volumes on the Docker host) — these fields tell the UI and the
          deployment docs which exports back each dir.
        </p>
        {field('NFS server', 'Host exporting the shares (e.g. unraid01 or 172.24.9.51).', 'nfs_server', 'unraid01')}
        {field('Scripts export', 'Export path for episode scripts/sources (e.g. /mnt/user/scripts).', 'scripts_share', '/mnt/user/scripts')}
        {field('Release export', 'Export path for finished releases (e.g. /mnt/user/ReleaseFolders).', 'release_share', '/mnt/user/ReleaseFolders')}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Controller roots (container mounts)</h3>
        {field('Scripts root', 'Where the scripts share is mounted in the container. The scanner watches this.', 'scripts_root', '/data/scripts')}
        {field('Release root', 'Where the release share is mounted in the container.', 'release_root', '/data/release')}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Remote path mapping (node-side)</h3>
        <p className="muted">
          Rendered job scripts run on the Windows nodes — these are the
          directories they use. Change them here when a node mounts the
          shares elsewhere; new jobs pick the mapping up immediately.
        </p>
        {field('Tools dir (bin)', 'Tools folder on nodes: x265, eac3to, opusenc, MediaInfo… e.g. C:\\bin', 'node_bin_dir', 'C:\\bin')}
        {field('Scripts dir on nodes', 'NFS scripts mount as seen by the node, e.g. C:\\Encodes\\scripts', 'node_scripts_dir', 'C:\\Encodes\\scripts')}
        {field('Release dir on nodes', 'NFS release mount as seen by the node, e.g. C:\\Encodes\\ReleaseFolders', 'node_release_dir', 'C:\\Encodes\\ReleaseFolders')}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Linux node paths (optional overrides)</h3>
        <p className="muted">
          POSIX paths used when a job renders for a Linux node. Empty = use the
          Windows path above (back-compat). S3-staged jobs ignore these (the
          per-job staging dir wins). Set these when Linux nodes mount shares
          or tools at different locations than the Windows nodes.
        </p>
        {field('Linux bin dir', 'Tools folder on Linux nodes, e.g. /opt/encode-bin. Empty = use the Windows path.', 'linux_bin_dir', '/opt/encode-bin')}
        {field('Linux scripts dir', 'Scripts mount on Linux nodes, e.g. /mnt/encode/scripts. Empty = use the Windows path.', 'linux_scripts_dir', '/mnt/encode/scripts')}
        {field('Linux release dir', 'Release mount on Linux nodes, e.g. /mnt/encode/release. Empty = use the Windows path.', 'linux_release_dir', '/mnt/encode/release')}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Behavior & naming</h3>
        <div className="toolbar">
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Scan interval (seconds)</span>
            <input
              type="number"
              min={5}
              max={3600}
              style={{ width: '100%' }}
              value={settings.scan_interval_seconds}
              disabled={!isAdmin}
            onChange={(e) => set('scan_interval_seconds', Number(e.target.value))}
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Tasks before reboot</span>
            <input
              type="number"
              min={1}
              max={1000}
              style={{ width: '100%' }}
              value={settings.tasks_before_reboot}
              disabled={!isAdmin}
            onChange={(e) => set('tasks_before_reboot', Number(e.target.value))}
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>
              Job retention (days, 0 = forever)
            </span>
            <input
              type="number"
              min={0}
              max={3650}
              style={{ width: '100%' }}
              value={settings.job_retention_days ?? 0}
              disabled={!isAdmin}
            onChange={(e) => set('job_retention_days', Number(e.target.value))}
              title="Terminal jobs older than this are pruned hourly. 0 keeps history forever."
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>
              Disk alert (GB free, 0 = off)
            </span>
            <input
              type="number"
              min={0}
              max={100000}
              style={{ width: '100%' }}
              value={settings.disk_alert_gb ?? 0}
              disabled={!isAdmin}
            onChange={(e) => set('disk_alert_gb', Number(e.target.value))}
              title="Nodes below this free space alert Discord (1/hour) and take no new jobs until they recover. 0 disables."
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>
              Stall timeout (minutes, 0 = off)
            </span>
            <input
              type="number"
              min={0}
              max={10080}
              style={{ width: '100%' }}
              value={settings.stall_timeout_minutes ?? 0}
              disabled={!isAdmin}
            onChange={(e) => set('stall_timeout_minutes', Number(e.target.value))}
              title="Running jobs reporting no step/progress change for this long are failed as stalled (flow retry policy applies). Set above the longest gap between progress updates of any step — heartbeats with unchanged progress do not count. 0 disables."
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Release group</span>
            <input
              style={{ width: '100%' }}
              value={settings.group}
              disabled={!isAdmin}
            onChange={(e) => set('group', e.target.value)}
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Quality tag</span>
            <input
              style={{ width: '100%' }}
              value={settings.tag}
              disabled={!isAdmin}
            onChange={(e) => set('tag', e.target.value)}
            />
          </label>
        </div>
        {/* Drain mode: finish running jobs, assign nothing. Use for host
            maintenance or before pushing bin packages. Live — the backend
            reads this flag on every idle heartbeat, so toggling it resumes
            assignment without a controller restart. */}
        <label style={{ display: 'flex', alignItems: 'center', gap: 8, margin: '12px 0' }}>
          <input
            type="checkbox"
            checked={!!settings.drain_mode}
            disabled={!isAdmin}
          onChange={(e) => set('drain_mode', e.target.checked)}
          />
          <span>Drain mode — finish running jobs, assign nothing</span>
        </label>
        <span className="muted" style={{ display: 'block', fontSize: 12 }}>
          Use for host maintenance or before pushing bin packages.
        </span>
        {isAdmin && (
        <button className="btn primary" disabled={saving} onClick={save} style={{ marginTop: 12 }}>
          {saving ? 'Saving…' : 'Save settings'}
        </button>
        )}
        {settings.updated_at && (
          <span className="muted" style={{ marginLeft: 12 }}>last saved {timeAgo(settings.updated_at)}</span>
        )}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Discord notifications</h3>
        <p className="muted">
          One webhook, two uses: the controller posts job-outcome alerts
          (done/failed) to it, and the <code>discord_notify</code> flow step
          falls back to it when a flow doesn't set its own webhook. Changes
          apply immediately — no restart. Leave blank to turn notifications
          off (overrides the controller's environment webhook).
        </p>
        {field(
          'Webhook URL',
          'https://discord.com/api/webhooks/… — validated on save.',
          'discord_webhook',
          'https://discord.com/api/webhooks/…',
        )}
        {/* Hourly digest (Phase F2): batch job outcomes into one summary
            message per hour instead of one message per job. Useful on busy
            fleets where per-job alerts would flood the channel. Saved via
            the existing settings PUT flow (same card's Save button). */}
        <label style={{ display: 'flex', alignItems: 'center', gap: 8, margin: '12px 0' }}>
          <input
            type="checkbox"
            checked={!!settings.notify_digest}
            disabled={!isAdmin}
          onChange={(e) => set('notify_digest', e.target.checked)}
          />
          <span>Hourly digest — batch job outcomes into one summary per hour</span>
        </label>
        <span className="muted" style={{ display: 'block', fontSize: 12 }}>
          Summarizes done/failed outcomes hourly; untick for per-job alerts.
        </span>
        {/* Node status alerts: post a Discord alert when a node transitions
            between online/offline/busy. Off by default; only fires when
            the webhook is set. Saved via the same PUT flow. */}
        <label style={{ display: 'flex', alignItems: 'center', gap: 8, margin: '12px 0' }}>
          <input
            type="checkbox"
            aria-label="Notify node status"
            checked={!!settings.notify_node_status}
            disabled={!isAdmin}
          onChange={(e) => set('notify_node_status', e.target.checked)}
          />
          <span>Node status alerts — Discord alert when a node goes offline or comes back</span>
        </label>
        <span className="muted" style={{ display: 'block', fontSize: 12 }}>
          Fires on online/offline/busy transitions; requires the webhook above.
        </span>
        {isAdmin && (
        <button className="btn primary" disabled={saving} onClick={save}>
          {saving ? 'Saving…' : 'Save settings'}
        </button>
        )}
        {settings.updated_at && (
          <span className="muted" style={{ marginLeft: 12 }}>last saved {timeAgo(settings.updated_at)}</span>
        )}
      </div>

      {/* Notification channels: additive transports for the same alerts the
          Discord card configures (job outcomes, node status, disk space,
          hourly digest). Each channel is off until configured and works
          with or without Discord. Secrets are write-only (SharesCard
          pattern): stored encrypted, never echoed back. */}
      <div className="card">
        <h3 style={{ marginTop: 0 }}>Notification channels (webhook · ntfy · email)</h3>
        <p className="muted">
          Extra transports for the same alerts — job outcomes, node status,
          disk space and the hourly digest. Each channel is off until
          configured; they work with or without Discord. Secrets are
          write-only: stored encrypted, never shown again — leave blank to
          keep the saved value.
        </p>
        {field(
          'Generic webhook URL',
          'Structured JSON event POSTs (job_finished / alert). http(s) only — payload shape in Docs/md/Operations.md.',
          'generic_webhook_url',
          'https://automation.example.com/hook',
        )}
        {field(
          'ntfy topic URL',
          'Full publish endpoint, e.g. https://ntfy.sh/encode-alerts (self-hosted servers work too).',
          'ntfy_topic_url',
          'https://ntfy.sh/encode-alerts',
        )}
        {secretField(
          'ntfy access token (optional)',
          'Bearer token for protected topics. Write-only — blank keeps the stored token.',
          ntfyToken, setNtfyToken, !!settings.has_ntfy_token, 'optional',
        )}
        {field('SMTP host', 'Mail relay hostname (no scheme). Enables email when host + from + to are all set.', 'smtp_host', 'mail.example.com')}
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 2 }}>SMTP port (0 = default 587)</span>
          <input
            type="number"
            min={0}
            max={65535}
            style={{ width: '100%' }}
            value={settings.smtp_port ?? 0}
            disabled={!isAdmin}
            onChange={(e) => set('smtp_port', Number(e.target.value))}
          />
          <span className="muted" style={{ fontSize: 12 }}>
            Submission port; authenticated relays must offer STARTTLS.
          </span>
        </label>
        {field('SMTP user (optional)', 'Leave blank for an unauthenticated relay.', 'smtp_user', 'encode-bot')}
        {secretField(
          'SMTP password',
          'Write-only — blank keeps the stored password.',
          smtpPassword, setSmtpPassword, !!settings.has_smtp_password, 'optional',
        )}
        {field('SMTP from', 'Envelope sender, e.g. encode@example.com.', 'smtp_from', 'encode@example.com')}
        {field('SMTP to', 'Comma-separated recipients, e.g. ops@example.com, oncall@example.com.', 'smtp_to', 'ops@example.com')}
        {isAdmin && (
        <button className="btn primary" disabled={saving} onClick={save}>
          {saving ? 'Saving…' : 'Save settings'}
        </button>
        )}
        {settings.updated_at && (
          <span className="muted" style={{ marginLeft: 12 }}>last saved {timeAgo(settings.updated_at)}</span>
        )}
      </div>

      {/* API tokens: scoped Bearer credentials for external automation
          (Sonarr-style triggers, Grafana, scripts). Managed independently of
          the settings blob — create/revoke take effect immediately. */}
      {/* Storage shares: nfs/smb/s3 sources per pipeline role. Replaces
          the legacy NFS settings fields (which the backend migrates into
          share rows on boot). */}
      {isAdmin && <SharesCard />}

      {isAdmin && <APITokensCard />}

      {/* Controller DB backups: scheduled VACUUM INTO snapshots with
          manual trigger + download + restore. Formalizes the pre-deploy
          `cp encode.db` habit. */}
      {isAdmin && <BackupsCard />}

      {/* User management: admin-only card for creating/deleting accounts
          and resetting passwords. Rendered only when the logged-in user is
          an admin (see useRole / RoleContext in App.tsx). */}
      {isAdmin && <UsersCard />}

      {/* Version + update check: current controller version with a
          best-effort "Check for update" button. A 502 upstream failure
          surfaces as a muted inline error — never an error-box. */}
      <div className="card">
        <h3 style={{ marginTop: 0 }}>Controller version</h3>
        <div className="toolbar" style={{ marginBottom: 0 }}>
          <span className="chip">
            <span className="chip-label">current</span>
            <span className="chip-value">{updateCheck?.current ?? '—'}</span>
          </span>
          <button
            className="btn"
            disabled={checking}
            onClick={checkForUpdate}
          >
            {checking ? 'Checking…' : 'Check for update'}
          </button>
          {updateCheck && updateCheck.update_available && (
            <span className="badge" style={{ background: 'var(--accent-soft)', color: 'var(--accent-hi)', borderColor: 'rgba(157,107,255,0.2)' }}>
              v{updateCheck.latest} available
            </span>
          )}
          {updateCheck && !updateCheck.update_available && (
            <span className="muted">Up to date</span>
          )}
          {updateError && <span className="muted">Update check failed</span>}
        </div>
        {updateCheck && updateCheck.update_available && (
          <div style={{ marginTop: 12 }}>
            <p className="muted" style={{ fontSize: 12, marginBottom: 6 }}>
              <a href={updateCheck.release_url} target="_blank" rel="noopener noreferrer">
                Release notes
              </a>
            </p>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <code style={{ fontSize: 11, flex: 1, wordBreak: 'break-all' }} data-testid="upgrade-cmd">
                curl -fsSL {updateCheck.release_url.replace('/tag/', '/download/')}/install.sh | sudo ENCODE_HOME=/opt/encode-system bash -s v{updateCheck.latest}
              </code>
              <button
                aria-label="Copy upgrade command"
                onClick={() => navigator.clipboard?.writeText(
                  `curl -fsSL ${updateCheck.release_url.replace('/tag/', '/download/')}/install.sh | sudo ENCODE_HOME=/opt/encode-system bash -s v${updateCheck.latest}`,
                )}
              >
                Copy
              </button>
            </div>
          </div>
        )}
      </div>

      {isAdmin && (
      <div className="card">
        <h3 style={{ marginTop: 0 }}>Push to nodes</h3>
        <p className="muted">
          Publish a payload here and every enabled node adopts it on its next
          idle heartbeat (SHA-256 verified on the node).
        </p>

        {/* Platform picker: segmented Windows/Linux control. Governs which
            agent slot publish/rollback targets and which manifest rows are
            highlighted. Lib/bin are shared across platforms (not slotted). */}
        <div className="toolbar" style={{ marginBottom: 12 }}>
          <div role="group" aria-label="Agent platform" data-testid="platform-picker">
            <button
              className={`btn${platform === 'windows' ? ' primary' : ''}`}
              aria-pressed={platform === 'windows'}
              onClick={() => setPlatform('windows')}
              style={{ borderRadius: '6px 0 0 6px' }}
            >
              <span className="chip-value">WIN</span>
            </button>
            <button
              className={`btn${platform === 'linux' ? ' primary' : ''}`}
              aria-pressed={platform === 'linux'}
              onClick={() => setPlatform('linux')}
              style={{ borderRadius: '0 6px 6px 0', marginLeft: -1 }}
            >
              <span className="chip-value">LINUX</span>
            </button>
          </div>
        </div>

        {/* Manifest rows: show BOTH Windows and Linux agent slots side by
            side. Mono chips for version strings. */}
        {manifest && (
          <div style={{ display: 'flex', gap: 16, marginBottom: 16, flexWrap: 'wrap' }}>
            <div>
              <span className="muted" style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.7, display: 'block', marginBottom: 4 }}>Windows agent</span>
              <span className="chip"><span className="chip-label">current</span><span className="chip-value">{manifest.agent_version || '—'}</span></span>
              {manifest.prev_agent_version && (
                <span className="chip" style={{ marginLeft: 4 }}><span className="chip-label">prev</span><span className="chip-value">{manifest.prev_agent_version}</span></span>
              )}
            </div>
            <div>
              <span className="muted" style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.7, display: 'block', marginBottom: 4 }}>Linux agent</span>
              <span className="chip"><span className="chip-label">current</span><span className="chip-value">{manifest.linux_agent_version || '—'}</span></span>
              {manifest.prev_linux_agent_version && (
                <span className="chip" style={{ marginLeft: 4 }}><span className="chip-label">prev</span><span className="chip-value">{manifest.prev_linux_agent_version}</span></span>
              )}
            </div>
            <div>
              <span className="muted" style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.7, display: 'block', marginBottom: 4 }}>Shared</span>
              <span className="chip"><span className="chip-label">lib</span><span className="chip-value">v{manifest.lib_version || '—'}</span></span>
              <span className="chip" style={{ marginLeft: 4 }}><span className="chip-label">bin</span><span className="chip-value">v{manifest.bin_version || '—'}{manifest.bin_size ? ` (${(manifest.bin_size / 1048576).toFixed(1)} MiB)` : ''}</span></span>
            </div>
          </div>
        )}

        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Agent version ({platform})</span>
            <input style={{ width: '100%' }} value={agentVersion} onChange={(e) => setAgentVersion(e.target.value)} placeholder="e.g. 0.8.0" />
          </label>
          <input ref={agentFileRef} type="file" accept={platform === 'linux' ? '.bin,application/octet-stream' : '.exe'} onChange={(e) => setAgentFile(e.target.files?.[0] ?? null)} />
          <button className="btn primary" disabled={publishing !== '' || !agentFile || !agentVersion.trim()} onClick={() => publish('agent')}>
            {publishing === 'agent' ? 'Publishing…' : 'Publish agent'}
          </button>
          {(platform === 'windows' ? manifest?.prev_agent_version : manifest?.prev_linux_agent_version) && (
            <button
              className="btn danger"
              disabled={publishing !== ''}
              onClick={rollback}
              title={`Promote the previous ${platform} release back to current; nodes self-downgrade on their next idle heartbeat`}
            >
              {publishing === 'rollback' ? 'Rolling back…' : `Rollback to ${platform === 'windows' ? manifest!.prev_agent_version : manifest!.prev_linux_agent_version}`}
            </button>
          )}
        </div>

        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>EncodeLib version (must exceed {manifest?.lib_version ?? 0})</span>
            <input style={{ width: '100%' }} value={libVersion} onChange={(e) => setLibVersion(e.target.value)} placeholder="e.g. 3" />
          </label>
          <input ref={libFileRef} type="file" accept=".ps1" onChange={(e) => setLibFile(e.target.files?.[0] ?? null)} />
          <button className="btn primary" disabled={publishing !== '' || !libFile || !libVersion} onClick={() => publish('lib')}>
            {publishing === 'lib' ? 'Publishing…' : 'Publish EncodeLib'}
          </button>
        </div>

        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Bin package version (must exceed {manifest?.bin_version ?? 0})</span>
            <input style={{ width: '100%' }} value={binVersion} onChange={(e) => setBinVersion(e.target.value)} placeholder="e.g. 1" />
          </label>
          <input ref={binFileRef} type="file" accept=".zip" onChange={(e) => setBinFile(e.target.files?.[0] ?? null)} />
          <button className="btn primary" disabled={publishing !== '' || !binFile || !binVersion} onClick={() => publish('bin')}>
            {publishing === 'bin' ? 'Publishing…' : 'Publish bin package'}
          </button>
        </div>
        <p className="muted">
          The bin package is a zip of the node tools folder (contents land
          directly in the node&apos;s bin dir). The zip is validated here at
          publish time — path traversal, absolute paths, and symlinks are
          rejected.
        </p>

        <h3 style={{ marginTop: 18 }}>…or fetch the bin package from a URL</h3>
        <p className="muted">
          The controller downloads the zip itself and runs the exact same
          validation. Typical source: a release asset on{' '}
          <code>github.com/badskater/encode-bin</code> — the version number
          must match the fleet&apos;s bin_version counter.
        </p>
        <label style={{ display: 'block', marginBottom: 8 }}>
          <span style={{ display: 'block', marginBottom: 2 }}>Download URL</span>
          <input
            style={{ width: '100%' }}
            value={binUrl}
            onChange={(e) => setBinUrl(e.target.value)}
            placeholder="https://github.com/badskater/encode-bin/releases/download/v4/bin-package.zip"
          />
        </label>
        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Version (must exceed {manifest?.bin_version ?? 0})</span>
            <input style={{ width: '100%' }} value={binUrlVersion} onChange={(e) => setBinUrlVersion(e.target.value)} placeholder="e.g. 4" />
          </label>
          <label style={{ flex: 2 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>SHA-256 (optional, from the release notes)</span>
            <input style={{ width: '100%' }} value={binUrlSha} onChange={(e) => setBinUrlSha(e.target.value)} placeholder="ca62575b…" />
          </label>
          <button
            className="btn primary"
            disabled={publishing !== '' || !binUrl.trim() || !/^\d+$/.test(binUrlVersion)}
            onClick={() => publish('bin-url')}
          >
            {publishing === 'bin-url' ? 'Fetching…' : 'Fetch & publish'}
          </button>
        </div>
      </div>
      )}
    </>
  );
}
