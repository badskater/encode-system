import { useState } from 'react';
import { api } from '../api/client';
import type { Flow, Series, SeriesTemplate } from '../types';
import { usePolling } from '../hooks/usePolling';
import CreateSeriesDialog from '../components/CreateSeriesDialog';
import DeleteSeriesDialog from '../components/DeleteSeriesDialog';
import { useIsAdmin } from '../hooks/useRole';

// SeriesPage manages per-series flow selection, tag overrides and enable
// state. Episodes of an enabled series are queued automatically by the
// scanner and distributed across all enabled idle nodes (one job per node).
export default function SeriesPage() {
  const { data: series, error, refresh } = usePolling<Series[]>(() => api.series(), 5000);
  const { data: flows } = usePolling<Flow[]>(() => api.flows(), 30000);
  const { data: settings } = usePolling<{ tag: string }>(() => api.settings(), 60000);
  const [actionError, setActionError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  // deleting holds the series whose registry-row deletion is being
  // confirmed; null = no dialog open. Admin-only (the action itself is
  // gated below, and the endpoint enforces withAdmin server-side).
  const [deleting, setDeleting] = useState<Series | null>(null);
  const isAdmin = useIsAdmin();

  async function selectFlow(sr: Series, flowId: number) {
    try {
      await api.patchSeries(sr.id, { flow_id: flowId });
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  async function changeTag(sr: Series, tag: string) {
    try {
      await api.patchSeries(sr.id, { tag });
      setActionError(null);
      refresh();
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  async function changeNodeGroup(sr: Series, nodeGroup: string) {
    try {
      await api.patchSeries(sr.id, { node_group: nodeGroup });
      setActionError(null);
      refresh();
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }
  // changeWebhook sets the per-series Discord webhook override; blank
  // returns the series to the global channel.
  async function changeWebhook(sr: Series, url: string) {
    try {
      await api.patchSeries(sr.id, { webhook_url: url });
      setActionError(null);
      refresh();
    } catch (e) {
      setActionError(String(e));
    }
  }

  // changeScript assigns (or clears) the series' filter-script template.
  // Both fields always travel together — the server rejects half
  // assignments with 400. Errors propagate to ScriptCell, which keeps the
  // editor open and shows the message inline (like DeleteSeriesDialog).
  async function changeScript(sr: Series, scriptType: string, scriptTemplate: string) {
    await api.patchSeries(sr.id, { script_type: scriptType, script_template: scriptTemplate });
    setActionError(null);
    refresh();
  }


  async function toggle(sr: Series) {
    try {
      await api.patchSeries(sr.id, { enabled: !sr.enabled });
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  // togglePause holds the series at BOTH gates: the scanner stops queueing
  // and already-pending jobs stop dispatching until unpause. Unlike the
  // enabled checkbox, pausing preserves queue position.
  async function togglePause(sr: Series) {
    try {
      await api.patchSeries(sr.id, { paused: !sr.paused });
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  // toggleNotify mutes/unmutes a series' Discord job-outcome alerts. Muted
  // series still queue and encode — only the alert is suppressed.
  async function toggleNotify(sr: Series) {
    try {
      await api.patchSeries(sr.id, { notify: !sr.notify });
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  const flowName = (id: number) => {
    if (id === 0) return null;
    return flows?.find((f) => f.id === id)?.name ?? `#${id}`;
  };

  return (
    <>
      <h2>Series</h2>
      {error && <div className="error-box">{error}</div>}
      {actionError && <div className="error-box">{actionError}</div>}

      <p className="muted">
        Series are registered automatically when the scanner first sees their
        folder. Pick the flow each series encodes with (or inherit the default
        flow), override the quality tag per series, and pause a series to stop
        new jobs without touching other series. Episodes are distributed
        across every enabled idle node — one episode per node at a time.
      </p>

      <div className="toolbar">
        {isAdmin && (
          <button className="btn primary" onClick={() => setShowCreate(true)}>
            Create series
          </button>
        )}
      </div>

      <table>
        <thead>
          <tr>
            <th>Series</th>
            <th>Encoding with</th>
            <th>Tag</th>
            <th>Nodes</th>
            <th>Progress</th>
            <th>Jobs</th>
            <th>Accepting work</th>
            <th>Paused</th>
            <th>Notify</th>
            <th>Channel</th>
            <th>Filter script</th>
            {isAdmin && <th>Actions</th>}
          </tr>
        </thead>
        <tbody>
          {(series ?? []).map((sr) => (
            <tr key={sr.id}>
              <td style={{ opacity: sr.paused ? 0.55 : sr.notify === false ? 0.7 : undefined }}>
                {sr.name}
              </td>
              <td>
                {isAdmin ? (
                  <select
                    value={sr.flow_id}
                    onChange={(e) => selectFlow(sr, Number(e.target.value))}
                  >
                    <option value={0}>
                      Default flow
                      {flows?.find((f) => f.is_default)
                        ? ` (${flows.find((f) => f.is_default)!.name})`
                        : ''}
                    </option>
                    {(flows ?? []).map((f) => (
                      <option key={f.id} value={f.id}>
                        {f.name}
                        {f.is_default ? ' (default)' : ''}
                      </option>
                    ))}
                  </select>
                ) : (
                  <span className="muted">{sr.flow_id ? flowName(sr.flow_id) : 'default'}</span>
                )}
                {sr.flow_id > 0 && (
                  <span className="muted"> → {flowName(sr.flow_id)}</span>
                )}
              </td>
              <td>
                <TagCell
                  tag={sr.tag}
                  globalTag={settings?.tag ?? ''}
                  onSave={(t) => changeTag(sr, t)}
                />
              </td>
              <td>
                <TagCell
                  tag={sr.node_group ?? ''}
                  globalTag="any node"
                  fallbackSuffix=""
                  title="Click to restrict this series to a node group (blank = any node)"
                  onSave={(g) => changeNodeGroup(sr, g)}
                />
              </td>
              <td>
                <ProgressCell
                  done={sr.episodes_done ?? 0}
                  failed={sr.episodes_failed ?? 0}
                  active={sr.episodes_active ?? 0}
                  total={sr.episodes_total ?? 0}
                />
              </td>
              <td>{sr.jobs ?? 0}</td>
              <td>
                {isAdmin ? (
                  <input type="checkbox" checked={sr.enabled} onChange={() => toggle(sr)} />
                ) : (
                  <input type="checkbox" checked={sr.enabled} disabled />
                )}
              </td>
              <td>
                {isAdmin ? (
                  <button
                    className="btn"
                    style={{ padding: '2px 10px', fontSize: 12 }}
                    title={sr.paused ? 'Resume scanning and dispatch' : 'Pause scanning and hold queued jobs'}
                    aria-label={sr.paused ? 'Resume' : 'Pause'}
                    onClick={() => togglePause(sr)}
                  >
                    {sr.paused ? 'Resume' : 'Pause'}
                  </button>
                ) : (
                  <span className="muted">{sr.paused ? 'paused' : 'running'}</span>
                )}
              </td>
              <td>
                {isAdmin ? (
                  <button
                    className="btn"
                    style={{ padding: '2px 10px', fontSize: 12 }}
                    title={sr.notify === false ? 'Unmute Discord alerts' : 'Mute Discord alerts'}
                    aria-label={sr.notify === false ? 'Unmute' : 'Mute'}
                    onClick={() => toggleNotify(sr)}
                  >
                    {sr.notify === false ? 'Muted' : 'Alerts'}
                  </button>
                ) : (
                  <span className="muted">{sr.notify === false ? 'muted' : 'alerts'}</span>
                )}
              </td>
              <td>
                <WebhookCell
                  url={sr.webhook_url ?? ''}
                  onSave={(u) => changeWebhook(sr, u)}
                />
              </td>
              <td>
                <ScriptCell
                  series={sr}
                  onSave={(t, tpl) => changeScript(sr, t, tpl)}
                />
              </td>
              {isAdmin && (
                <td>
                  <button
                    className="btn danger"
                    style={{ padding: '2px 10px', fontSize: 12 }}
                    title="Remove the registry row — jobs, history and files are kept"
                    aria-label={`Delete ${sr.name}`}
                    onClick={() => setDeleting(sr)}
                  >
                    Delete
                  </button>
                </td>
              )}
            </tr>
          ))}
          {(series ?? []).length === 0 && (
            <tr>
              <td colSpan={isAdmin ? 12 : 11} className="muted">
                No series yet — use Create series, or drop a series folder into
                the scripts share and the scanner will register it.
              </td>
            </tr>
          )}
        </tbody>
      </table>

      {showCreate && (
        <CreateSeriesDialog
          flows={flows ?? []}
          onClose={() => {
            setShowCreate(false);
            refresh();
          }}
        />
      )}

      {deleting && (
        <DeleteSeriesDialog
          series={deleting}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            setDeleting(null);
            refresh();
          }}
        />
      )}
    </>
  );
}

// TagCell edits a series' quality-tag override inline. Blank means "inherit
// the global settings tag"; the placeholder shows which one is inherited.
function TagCell({
  tag,
  globalTag,
  onSave,
  title = 'Click to set a per-series tag override',
  fallbackSuffix = ' (global)',
}: {
  tag: string;
  globalTag: string;
  onSave: (t: string) => void;
  title?: string;
  // Text appended to the inherited value shown for a blank cell. Tags
  // inherit the global settings tag ("1080p (global)"); groups don't
  // inherit anything, so the group cell passes "".
  fallbackSuffix?: string;
}) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(tag);

  if (!editing) {
    return (
      <span
        className="muted"
        style={{ cursor: 'pointer' }}
        title={title}
        onClick={() => {
          setValue(tag);
          setEditing(true);
        }}
      >
        {tag || <em>{globalTag}{fallbackSuffix}</em>}
      </span>
    );
  }
  return (
    <input
      type="text"
      style={{ width: 90 }}
      value={value}
      placeholder={globalTag}
      autoFocus
      onChange={(e) => setValue(e.target.value)}
      onBlur={() => {
        setEditing(false);
        if (value.trim() !== tag) onSave(value.trim());
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
          setEditing(false);
          if (value.trim() !== tag) onSave(value.trim());
        }
        if (e.key === 'Escape') {
          setEditing(false);
        }
      }}
    />
  );
}

// ProgressCell renders a compact per-series encode progress summary:
// "done/total" (e.g. 12/24) with a small fill bar whose width is done/total
// %. When there are failed or in-flight episodes it appends a muted line
// "N failed · N active". A series whose total denominator is 0 (no jobs
// history and no scaffolded episode folders) shows muted "no jobs yet".
// Reuses the .step-bar-track/.step-bar-fill classes from styles.css.
function ProgressCell({
  done,
  failed,
  active,
  total,
}: {
  done: number;
  failed: number;
  active: number;
  total: number;
}) {
  if (total === 0) {
    return <span className="muted">no jobs yet</span>;
  }
  const pct = Math.min(100, Math.round((done / total) * 100));
  const extra: string[] = [];
  if (failed > 0) extra.push(`${failed} failed`);
  if (active > 0) extra.push(`${active} active`);
  return (
    <div>
      <div className="step-bar-track" title={`${done}/${total} episodes encoded`}>
        <div className="step-bar-fill" style={{ width: `${pct}%` }} />
      </div>
      <span>{done}/{total}</span>
      {extra.length > 0 && <div className="muted">{extra.join(' · ')}</div>}
    </div>
  );
}

// WebhookCell shows the per-series alert-channel state ("global" when no
// override) and expands to a URL input on click. https-only is enforced
// server-side; the client mirrors the rule so typos fail fast with a
// visible reason instead of a 400 toast.
function WebhookCell({ url, onSave }: { url: string; onSave: (u: string) => void }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(url);

  if (!editing) {
    return (
      <span
        className="muted"
        style={{ cursor: 'pointer' }}
        title={url ? `Own channel: ${url}` : 'Alerts go to the global webhook — click to set a per-series channel'}
        onClick={() => {
          setValue(url);
          setEditing(true);
        }}
      >
        {url ? '📣 own' : <em>global</em>}
      </span>
    );
  }
  return (
    <input
      type="url"
      aria-label="Series webhook URL"
      style={{ width: 180 }}
      value={value}
      placeholder="https://discord.com/api/webhooks/…"
      autoFocus
      onChange={(e) => setValue(e.target.value)}
      onBlur={() => {
        setEditing(false);
        const v = value.trim();
        if (v !== url && (v === '' || v.startsWith('https://'))) onSave(v);
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
          setEditing(false);
          const v = value.trim();
          if (v !== url && (v === '' || v.startsWith('https://'))) onSave(v);
        }
        if (e.key === 'Escape') {
          setEditing(false);
        }
      }}
    />
  );
}

// ScriptCell shows the series' filter-script assignment (the template file
// name, or "—" when unassigned) and expands to a type + template picker on
// click, mirroring the TagCell/WebhookCell click-to-edit pattern. Templates
// come from GET /api/series/{id}/templates and are re-fetched when the
// editor opens or the type changes. Save always sends script_type and
// script_template TOGETHER (both '' for None) — the server rejects half
// assignments, so Save stays disabled until a template is picked. A failed
// save keeps the editor open and shows the server's error inline (like
// DeleteSeriesDialog).
function ScriptCell({
  series,
  onSave,
}: {
  series: Series;
  onSave: (scriptType: string, scriptTemplate: string) => Promise<void>;
}) {
  const [editing, setEditing] = useState(false);
  const [scriptType, setScriptType] = useState(series.script_type ?? '');
  const [template, setTemplate] = useState(series.script_template ?? '');
  const [templates, setTemplates] = useState<SeriesTemplate[]>([]);
  const [loadError, setLoadError] = useState('');
  const [saveError, setSaveError] = useState('');
  const [busy, setBusy] = useState(false);

  // loadTemplates refreshes the picker from the scripts share; a listing
  // failure degrades to an empty picker with a visible reason.
  async function loadTemplates() {
    try {
      const res = await api.seriesTemplates(series.id);
      setTemplates(res.templates);
      setLoadError('');
    } catch (e) {
      setTemplates([]);
      setLoadError(e instanceof Error ? e.message : String(e));
    }
  }

  async function save() {
    if (busy) return; // double-click guard
    setBusy(true);
    setSaveError('');
    try {
      await onSave(scriptType, scriptType === '' ? '' : template);
      setEditing(false);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  if (!editing) {
    return (
      <span
        className="muted"
        style={{ cursor: 'pointer' }}
        title={
          series.script_template
            ? `Filter script: ${series.script_template} — click to change`
            : 'No filter script assigned — click to pick a template'
        }
        onClick={() => {
          setScriptType(series.script_type ?? '');
          setTemplate(series.script_template ?? '');
          setSaveError('');
          setEditing(true);
          loadTemplates();
        }}
      >
        {series.script_template || <em>—</em>}
      </span>
    );
  }

  // Keep the current assignment selectable even if its file vanished from
  // the share since it was assigned (re-saving it surfaces the 400 inline).
  const names = templates.map((t) => t.name);
  if (template && !names.includes(template)) names.unshift(template);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
      <select
        aria-label="Filter script type"
        value={scriptType}
        onChange={(e) => {
          const v = e.target.value;
          setScriptType(v);
          setTemplate(''); // extension changes with the type — re-pick
          if (v !== '') loadTemplates();
        }}
      >
        <option value="">None</option>
        <option value="vpy">VapourSynth (.vpy)</option>
        <option value="avs">AviSynth (.avs)</option>
      </select>
      {scriptType !== '' && (
        <select
          aria-label="Filter script template"
          value={template}
          onChange={(e) => setTemplate(e.target.value)}
        >
          <option value="">Choose template…</option>
          {names.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      )}
      {loadError && <div className="error-box">{loadError}</div>}
      {scriptType !== '' && !loadError && names.length === 0 && (
        <span className="muted" style={{ fontSize: 12 }}>
          No templates in _templates/{series.name}/ yet.
        </span>
      )}
      {saveError && <div className="error-box">{saveError}</div>}
      <div style={{ display: 'flex', gap: 4 }}>
        <button
          className="btn primary"
          style={{ padding: '2px 10px', fontSize: 12 }}
          disabled={busy || (scriptType !== '' && template === '')}
          onClick={save}
        >
          {busy ? 'Saving…' : 'Save'}
        </button>
        <button
          className="btn"
          style={{ padding: '2px 10px', fontSize: 12 }}
          onClick={() => setEditing(false)}
        >
          Cancel
        </button>
      </div>
    </div>
  );
}
