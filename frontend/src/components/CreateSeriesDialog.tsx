import { useState } from 'react';
import { api } from '../api/client';
import type { CreateSeriesResponse, Flow } from '../types';

// CreateSeriesDialog scaffolds a new series on the shares: episode folders
// (Ep 01 … Ep NN) in the scripts share, the release folder on the release
// share, and registers the series row. The operator then drops sources and
// filter scripts into each episode folder; the scanner queues jobs once both
// are present.
export default function CreateSeriesDialog({
  flows,
  onClose,
}: {
  flows: Flow[];
  onClose: () => void;
}) {
  const [name, setName] = useState('');
  const [episodes, setEpisodes] = useState(12);
  const [tag, setTag] = useState(''); // blank = inherit the global settings tag
  const [flowId, setFlowId] = useState(0); // 0 = default flow
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<CreateSeriesResponse | null>(null);

  async function submit() {
    if (busy || done) return; // Enter key-repeat / double-click guard
    setError('');
    if (!name.trim()) {
      setError('Series name is required.');
      return;
    }
    if (!Number.isInteger(episodes) || episodes < 1 || episodes > 999) {
      setError('Episode count must be between 1 and 999.');
      return;
    }
    setBusy(true);
    try {
      const res = await api.createSeries({
        name: name.trim(),
        episodes,
        tag: tag.trim() || undefined,
        flow_id: flowId || undefined,
      });
      setDone(res);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Series creation failed');
    } finally {
      setBusy(false);
    }
  }

  if (done) {
    return (
      <div className="modal-backdrop" onClick={onClose} role="dialog" aria-modal="true">
        <div
          className="card modal"
          onClick={(e) => e.stopPropagation()}
          style={{ maxWidth: 560 }}
        >
          <h3>Series created</h3>
          <p className="muted">
            Folder structure is ready. Drop a source file and a filter script
            (1080.avs/.vpy or 2160.avs/.vpy) into each episode folder — the
            scanner queues jobs once both are present.
          </p>
          <div style={{ fontSize: 13, lineHeight: 1.7 }}>
            <div>
              <span className="muted">Series: </span>
              <strong>{done.series.name}</strong>
            </div>
            <div>
              <span className="muted">Episodes: </span>
              {done.episode_folders.length} ({done.episode_folders[0]} …{' '}
              {done.episode_folders[done.episode_folders.length - 1]})
            </div>
            <div style={{ wordBreak: 'break-all' }}>
              <span className="muted">Episode folders: </span>
              <code>{done.scripts_folder}</code>
            </div>
            {done.release_folder && (
              <div style={{ wordBreak: 'break-all' }}>
                <span className="muted">Release folder: </span>
                <code>{done.release_folder}</code>
              </div>
            )}
          </div>
          <div className="toolbar">
            <button className="btn primary" onClick={onClose}>
              Close
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div
      className="modal-backdrop"
      onClick={() => !busy && onClose()}
      role="dialog"
      aria-modal="true"
    >
      <div
        className="card modal"
        onClick={(e) => e.stopPropagation()}
        style={{ maxWidth: 460 }}
      >
        <h3>Create series</h3>
        <p className="muted">
          Creates the episode folders on the scripts share and the release
          folder on the release share.
        </p>
        {error && <div className="error-box">{error}</div>}
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>Series name</span>
          <input
            type="text"
            style={{ width: '100%' }}
            placeholder="e.g. Ookami-san to Shichinin no Nakama-tachi"
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
        </label>
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>Number of episodes</span>
          <input
            type="number"
            min={1}
            max={999}
            style={{ width: 120 }}
            value={episodes}
            onChange={(e) => setEpisodes(Number(e.target.value))}
          />
        </label>
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>
            Quality tag (blank = global tag from Settings)
          </span>
          <input
            type="text"
            style={{ width: '100%' }}
            placeholder="e.g. 2160p"
            value={tag}
            onChange={(e) => setTag(e.target.value)}
          />
        </label>
        <label style={{ display: 'block', marginBottom: 12 }}>
          <span style={{ display: 'block', marginBottom: 4 }}>Flow</span>
          <select style={{ width: '100%' }} value={flowId} onChange={(e) => setFlowId(Number(e.target.value))}>
            <option value={0}>
              Default flow
              {flows.find((f) => f.is_default) ? ` (${flows.find((f) => f.is_default)!.name})` : ''}
            </option>
            {flows.map((f) => (
              <option key={f.id} value={f.id}>
                {f.name}
                {f.is_default ? ' (default)' : ''}
              </option>
            ))}
          </select>
        </label>
        <div className="toolbar">
          <button className="btn primary" disabled={busy} onClick={submit}>
            {busy ? 'Creating…' : 'Create series'}
          </button>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}
