import { useEffect, useState } from 'react';
import { api } from '../api/client';
import type { NodeMetricSample } from '../types';
import Sparkline from './Sparkline';

// NodeMetricsPanel renders the per-node metric history for CPU and GPU
// utilization as sparklines, plus the latest encode fps. It lives as an
// inline expandable card under a node row (same .card styling as the Jobs
// detail card) rather than a modal, so the operator can scan the fleet
// table and a selected node's trend at the same time.
//
// The range picker (1h / 6h / 24h buttons) drives a fresh
// api.getNodeMetrics fetch; the panel owns its own loading + error state.
const RANGES = ['1h', '6h', '24h'] as const;
type Range = (typeof RANGES)[number];

export default function NodeMetricsPanel({
  nodeId,
  nodeName,
  onClose,
}: {
  nodeId: number;
  nodeName: string;
  onClose: () => void;
}) {
  const [range, setRange] = useState<Range>('1h');
  const [samples, setSamples] = useState<NodeMetricSample[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);

  // Fetch on mount and whenever the range changes. The mounted guard keeps
  // a late response from setting state after unmount (same pattern as
  // JobLogDialog).
  useEffect(() => {
    let mounted = true;
    setBusy(true);
    setError(null);
    api
      .getNodeMetrics(nodeId, range)
      .then((rows) => {
        if (mounted) {
          setSamples(rows);
          setError(null);
        }
      })
      .catch((e) => {
        if (mounted)
          setError(e instanceof Error ? e.message : 'failed to load metrics');
      })
      .finally(() => {
        if (mounted) setBusy(false);
      });
    return () => {
      mounted = false;
    };
  }, [nodeId, range]);

  // Latest sample drives the "current fps" readout. Sanitize non-finite.
  const latest = samples && samples.length > 0 ? samples[samples.length - 1] : null;
  const latestFps = latest ? latest.encode_fps : null;

  // Sparkline value arrays — coerce non-finite to 0 at the Sparkline layer
  // too (belt-and-suspenders), but filter nothing out: every sample is a
  // point on the timeline.
  const cpuValues = (samples ?? []).map((s) => s.cpu_pct);
  const gpuValues = (samples ?? [])
    .map((s) => s.gpu_util)
    .filter((v) => Number.isFinite(v) && v >= 0);

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h3 style={{ marginTop: 0 }}>
        {nodeName} — metrics
      </h3>

      <div className="toolbar" style={{ marginBottom: 12 }}>
        {RANGES.map((r) => (
          <button
            key={r}
            className={`btn ${range === r ? 'primary' : ''}`}
            onClick={() => setRange(r)}
            aria-pressed={range === r}
          >
            {r}
          </button>
        ))}
        <span style={{ flex: 1 }} />
        <button className="btn" onClick={onClose}>
          Close
        </button>
      </div>

      {busy && <p className="muted">Loading metrics…</p>}
      {error && <div className="error-box">{error}</div>}

      {!busy && !error && samples && samples.length === 0 && (
        <p className="muted">No metric samples recorded in this range.</p>
      )}

      {!busy && !error && samples && samples.length > 0 && (
        <>
          <div style={{ marginBottom: 12 }}>
            <Sparkline values={cpuValues} label="CPU" unit="%" width={240} height={48} />
          </div>
          {gpuValues.length > 0 && (
            <div style={{ marginBottom: 12 }}>
              <Sparkline values={gpuValues} label="GPU util" unit="%" width={240} height={48} />
            </div>
          )}
          <p className="muted">
            Latest encode fps: {latestFps !== null && latestFps > 0
              ? latestFps.toFixed(1)
              : 'idle'}
          </p>
        </>
      )}
    </div>
  );
}
