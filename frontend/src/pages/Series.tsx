import { useState } from 'react';
import { api } from '../api/client';
import type { Flow, Series } from '../types';
import { usePolling } from '../hooks/usePolling';

// SeriesPage manages per-series flow selection and enable state. Episodes of
// an enabled series are queued automatically by the scanner and distributed
// across all enabled idle nodes (one job per node).
export default function SeriesPage() {
  const { data: series, error } = usePolling<Series[]>(() => api.series(), 5000);
  const { data: flows } = usePolling<Flow[]>(() => api.flows(), 30000);
  const [actionError, setActionError] = useState<string | null>(null);

  async function selectFlow(sr: Series, flowId: number) {
    try {
      await api.patchSeries(sr.id, { flow_id: flowId });
      setActionError(null);
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
        Series are registered automatically when the scanner first sees their folder.
        Pick the flow each series encodes with (or inherit the default flow) and
        pause a series to stop new jobs without touching other series. Episodes are
        distributed across every enabled idle node — one episode per node at a time.
      </p>

      <table>
        <thead>
          <tr>
            <th>Series</th>
            <th>Encoding with</th>
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
              <td>{sr.jobs ?? 0}</td>
              <td>
                <input type="checkbox" checked={sr.enabled} onChange={() => toggle(sr)} />
              </td>
            </tr>
          ))}
          {(series ?? []).length === 0 && (
            <tr>
              <td colSpan={4} className="muted">
                No series yet — drop a series folder into the scripts share and the
                scanner will register it.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </>
  );
}
