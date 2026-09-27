// JobMetricsView renders ENCODE_METRIC key=value pairs reported by the job
// script (quality + output stats). Known keys get friendly labels, unit
// formatting, and warn styling on bad values; unknown keys render with a
// humanized raw name so new metrics from updated scripts appear without a
// frontend change.
const KNOWN: Record<
  string,
  { label: string; fmt: (v: number) => string; warn?: (v: number) => boolean }
> = {
  // VMAF below 90 is visibly degraded for anime sources — flag it so a bad
  // encode settings change is spotted at a glance (the whole point of the
  // metric).
  vmaf: { label: 'VMAF', fmt: (v) => v.toFixed(2), warn: (v) => v < 90 },
  output_bitrate_kbps: { label: 'Bitrate', fmt: (v) => `${Math.round(v).toLocaleString()} kb/s` },
  source_bitrate_kbps: { label: 'Src bitrate', fmt: (v) => `${Math.round(v).toLocaleString()} kb/s` },
  duration_sec: {
    label: 'Duration',
    fmt: (v) => {
      const s = Math.round(v);
      const h = Math.floor(s / 3600);
      const m = Math.floor((s % 3600) / 60);
      const sec = s % 60;
      return h > 0 ? `${h}h ${String(m).padStart(2, '0')}m` : `${m}m ${String(sec).padStart(2, '0')}s`;
    },
  },
  output_size_mb: { label: 'Out size', fmt: (v) => `${v.toLocaleString(undefined, { maximumFractionDigits: 1 })} MB` },
  source_size_mb: { label: 'Src size', fmt: (v) => `${v.toLocaleString(undefined, { maximumFractionDigits: 1 })} MB` },
};

// humanizeKey turns "my_custom_metric" into "My custom metric" for unknown
// keys — readable without a lookup table entry.
function humanizeKey(k: string): string {
  const s = k.replace(/_+/g, ' ').replace(/\s+/g, ' ').trim();
  return s.charAt(0).toUpperCase() + s.slice(1);
}

export default function JobMetricsView({ metrics }: { metrics?: Record<string, number> }) {
  if (!metrics) return null;
  const entries = Object.entries(metrics).filter(([, v]) => Number.isFinite(v));
  if (entries.length === 0) return null;
  // Known keys first (in table order), then unknown keys alphabetically —
  // deterministic rendering regardless of JSON key order from the backend.
  const knownOrder = Object.keys(KNOWN);
  entries.sort((a, b) => {
    const ia = knownOrder.indexOf(a[0]);
    const ib = knownOrder.indexOf(b[0]);
    if (ia >= 0 && ib >= 0) return ia - ib;
    if (ia >= 0) return -1;
    if (ib >= 0) return 1;
    return a[0].localeCompare(b[0]);
  });
  return (
    <>
      <h4>Encode metrics</h4>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {entries.map(([k, v]) => {
          const known = KNOWN[k];
          const label = known ? known.label : humanizeKey(k);
          const value = known ? known.fmt(v) : String(v);
          const isWarn = known?.warn ? known.warn(v) : false;
          return (
            <span key={k} className={`chip${isWarn ? ' warn' : ''}`} title={k}>
              <span className="chip-label">{label}</span>
              <span className="chip-value">{value}</span>
            </span>
          );
        })}
      </div>
    </>
  );
}
