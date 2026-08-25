import { useEffect, useState } from 'react';
import { api } from '../api/client';
import type { Settings, UpdateManifest } from '../types';
import { timeAgo } from '../components/helpers';

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
  const [publishing, setPublishing] = useState('');

  async function load() {
    try {
      const [st, m] = await Promise.all([api.settings(), api.manifest()]);
      setSettings(st);
      setManifest(m);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
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

  async function save() {
    if (!settings) return;
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      const saved = await api.saveSettings(settings);
      setSettings(saved);
      setNotice('Settings saved. Scanner and new jobs pick them up on the next cycle — no restart needed.');
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setSaving(false);
    }
  }

  async function publish(kind: 'agent' | 'lib' | 'bin') {
    setError(null);
    setNotice(null);
    setPublishing(kind);
    try {
      if (kind === 'agent') {
        if (!agentFile || !agentVersion.trim()) throw new Error('agent version + file are required');
        await api.publishAgent(agentVersion.trim(), agentFile);
        setAgentFile(null);
        setAgentVersion('');
      } else if (kind === 'lib') {
        if (!libFile || !/^\d+$/.test(libVersion)) throw new Error('lib needs a numeric version + file');
        await api.publishLib(Number(libVersion), libFile);
        setLibFile(null);
        setLibVersion('');
      } else {
        if (!binFile || !/^\d+$/.test(binVersion)) throw new Error('bin needs a numeric version + file');
        await api.publishBin(Number(binVersion), binFile);
        setBinFile(null);
        setBinVersion('');
      }
      setNotice(`${kind} published — idle nodes adopt it on their next heartbeat.`);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\d+:\s*/, '') : String(e));
    } finally {
      setPublishing('');
    }
  }

  const field = (label: string, hint: string, key: keyof Settings, placeholder = '') => (
    <label style={{ display: 'block', marginBottom: 12 }}>
      <span style={{ display: 'block', marginBottom: 2 }}>{label}</span>
      <input
        style={{ width: '100%' }}
        value={String(settings[key] ?? '')}
        placeholder={placeholder}
        onChange={(e) => set(key, e.target.value as never)}
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
          <input type="file" accept=".exe" onChange={(e) => setAgentFile(e.target.files?.[0] ?? null)} />
          <button className="btn primary" disabled={publishing !== '' || !agentFile || !agentVersion.trim()} onClick={() => publish('agent')}>
            {publishing === 'agent' ? 'Publishing…' : 'Publish agent'}
          </button>
        </div>

        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>EncodeLib version (must exceed {manifest?.lib_version ?? 0})</span>
            <input style={{ width: '100%' }} value={libVersion} onChange={(e) => setLibVersion(e.target.value)} placeholder="e.g. 3" />
          </label>
          <input type="file" accept=".ps1" onChange={(e) => setLibFile(e.target.files?.[0] ?? null)} />
          <button className="btn primary" disabled={publishing !== '' || !libFile || !libVersion} onClick={() => publish('lib')}>
            {publishing === 'lib' ? 'Publishing…' : 'Publish EncodeLib'}
          </button>
        </div>

        <div className="toolbar" style={{ alignItems: 'flex-end' }}>
          <label style={{ flex: 1 }}>
            <span style={{ display: 'block', marginBottom: 2 }}>Bin package version (must exceed {manifest?.bin_version ?? 0})</span>
            <input style={{ width: '100%' }} value={binVersion} onChange={(e) => setBinVersion(e.target.value)} placeholder="e.g. 1" />
          </label>
          <input type="file" accept=".zip" onChange={(e) => setBinFile(e.target.files?.[0] ?? null)} />
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
      </div>
    </>
  );
}
