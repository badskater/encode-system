import { useState } from 'react';
import { api } from '../api/client';
import type { Flow, Series } from '../types';
import { usePolling } from '../hooks/usePolling';
import CreateSeriesDialog from '../components/CreateSeriesDialog';

// SeriesPage manages per-series flow selection, tag overrides and enable
// state. Episodes of an enabled series are queued automatically by the
// scanner and distributed across all enabled idle nodes (one job per node).
export default function SeriesPage() {
  const { data: series, error, refresh } = usePolling<Series[]>(() => api.series(), 5000);
  const { data: flows } = usePolling<Flow[]>(() => api.flows(), 30000);
  const { data: settings } = usePolling<{ tag: string }>(() => api.settings(), 60000);
  const [actionError, setActionError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);

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

  async function toggle(sr: Series) {
    try {
      await api.patchSeries(sr.id, { enabled: !sr.enabled });
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
        <button className="btn primary" onClick={() => setShowCreate(true)}>
          Create series
        </button>
      </div>

      <table>
        <thead>
          <tr>
            <th>Series</th>
            <th>Encoding with</th>
            <th>Tag</th>
            <th>Jobs</th>
            <th>Accepting work</th>
          </tr>
        </thead>
        <tbody>
          {(series ?? []).map((sr) => (
            <tr key={sr.id}>
              <td>{sr.name}</td>
              <td>
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
              <td>{sr.jobs ?? 0}</td>
              <td>
                <input type="checkbox" checked={sr.enabled} onChange={() => toggle(sr)} />
              </td>
            </tr>
          ))}
          {(series ?? []).length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
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
    </>
  );
}

// TagCell edits a series' quality-tag override inline. Blank means "inherit
// the global settings tag"; the placeholder shows which one is inherited.
function TagCell({
  tag,
  globalTag,
  onSave,
}: {
  tag: string;
  globalTag: string;
  onSave: (t: string) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(tag);

  if (!editing) {
    return (
      <span
        className="muted"
        style={{ cursor: 'pointer' }}
        title="Click to set a per-series tag override"
        onClick={() => {
          setValue(tag);
          setEditing(true);
        }}
      >
        {tag || <em>{globalTag} (global)</em>}
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
