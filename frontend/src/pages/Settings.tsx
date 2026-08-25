import { useEffect, useRef, useState } from 'react';
import { api } from '../api/client';
import type { Settings, UpdateManifest } from '../types';
import { timeAgo } from '../components/helpers';

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
      };
      const saved = await api.saveSettings(toSave);
      setSettings(saved);
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
        await api.publishAgent(agentVersion.trim(), agentFile);
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

  const field = (label: string, hint: string, key: StringSettingKey, placeholder = '') => (
    <label style={{ display: 'block', marginBottom: 12 }}>
      <span style={{ display: 'block', marginBottom: 2 }}>{label}</span>
      <input
        style={{ width: '100%' }}
        value={String(settings[key] ?? '')}
        placeholder={placeholder}
        onChange={(e) => setString(key, e.target.value)}
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
              onChange={(e) => set('tasks_before_reboot', Number(e.target.value))}
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Release group</span>
            <input
              style={{ width: '100%' }}
              value={settings.group}
              onChange={(e) => set('group', e.target.value)}
            />
          </label>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Quality tag</span>
            <input
              style={{ width: '100%' }}
              value={settings.tag}
              onChange={(e) => set('tag', e.target.value)}
            />
          </label>
        </div>
        <button className="btn primary" disabled={saving} onClick={save}>
          {saving ? 'Saving…' : 'Save settings'}
        </button>
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
        <button className="btn primary" disabled={saving} onClick={save}>
          {saving ? 'Saving…' : 'Save settings'}
        </button>
        {settings.updated_at && (
          <span className="muted" style={{ marginLeft: 12 }}>last saved {timeAgo(settings.updated_at)}</span>
        )}
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Push to nodes</h3>
        <p className="muted">
          Publish a payload here and every enabled node adopts it on its next
          idle heartbeat (SHA-256 verified on the node). Current manifest:{' '}
          {manifest
            ? `agent ${manifest.agent_version || '—'} · lib v${manifest.lib_version || '—'} · bin v${manifest.bin_version || '—'}${manifest.bin_size ? ` (${(manifest.bin_size / 1048576).toFixed(1)} MiB)` : ''}`
            : 'loading…'}
        </p>

        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Agent version</span>
            <input style={{ width: '100%' }} value={agentVersion} onChange={(e) => setAgentVersion(e.target.value)} placeholder="e.g. 0.8.0" />
          </label>
          <input ref={agentFileRef} type="file" accept=".exe" onChange={(e) => setAgentFile(e.target.files?.[0] ?? null)} />
          <button className="btn primary" disabled={publishing !== '' || !agentFile || !agentVersion.trim()} onClick={() => publish('agent')}>
            {publishing === 'agent' ? 'Publishing…' : 'Publish agent'}
          </button>
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
    </>
  );
}
