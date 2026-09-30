import { useEffect, useState } from 'react';
import { api } from '../api/client';
import type { Stats } from '../types';

// Stats page: the fleet-wide aggregate job-history view (Phase E). Shows
// terminal counts + avg duration, per-node and per-flow breakdowns, the
// top failing steps, and a per-day done-throughput trend — all derived in
// SQL on the backend from the existing jobs columns. A range picker
// (24h/7d/30d/all) drives a fresh fetch; the page owns its loading + error
// state. Summary cards mirror the Dashboard Stat styling; the per-day trend
// is simple CSS bars (no chart library — YAGNI for ≤30 buckets).
const RANGES = ['24h', '7d', '30d', 'all'] as const;
type Range = (typeof RANGES)[number];

export default function StatsPage() {
  const [range, setRange] = useState<Range>('7d');
  const [stats, setStats] = useState<Stats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);

  // Fetch on mount and whenever the range changes. The mounted guard keeps a
  // late response from setting state after unmount (same pattern as
  // NodeMetricsPanel).
  useEffect(() => {
    let mounted = true;
    setBusy(true);
    setError(null);
    api
      .getStats(range)
      .then((s) => {
        if (mounted) setStats(s);
      })
      .catch((e) => {
        if (mounted) setError(e instanceof Error ? e.message : 'failed to load stats');
      })
      .finally(() => {
        if (mounted) setBusy(false);
      });
    return () => {
      mounted = false;
    };
  }, [range]);

  return (
    <>
      <h2>Stats</h2>

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
      </div>

      {busy && <p className="muted">Loading stats…</p>}
      {error && <div className="error-box">{error}</div>}

      {!busy && !error && stats && (
        <>
          {/* Summary cards: terminal counts + avg duration. Match the
              Dashboard Stat card styling (badge tones) so the fleet
              at-a-glance reads consistently across pages. */}
          <div className="card" style={{ display: 'flex', gap: 24, flexWrap: 'wrap', alignItems: 'center' }}>
            <Stat label="Done" value={stats.totals.done} tone="green" />
            <Stat label="Failed" value={stats.totals.failed} tone="blue" />
            <Stat label="Cancelled" value={stats.totals.cancelled} />
            <Stat
              label="Avg duration"
              value={humanizeSeconds(stats.totals.avg_duration_sec)}
              title="Average wall-clock duration of finished jobs (started_at → finished_at) in range"
            />
            <Stat
              label="Avg speedup"
              value={formatSpeedup(stats.totals.avg_speedup)}
              title="Media seconds encoded per wall-clock second (jobs reporting a duration_sec metric). Higher = faster than realtime."
            />
          </div>

          {/* Stuck episodes: failed jobs that burned at least one auto-retry.
              Triage list — worst-first, capped at 25 server-side. */}
          <div className="card">
            <h3 style={{ marginTop: 0 }}>Stuck episodes (repeated failures)</h3>
            <table>
              <thead>
                <tr>
                  <th>Episode</th>
                  <th>Attempts</th>
                  <th>Node</th>
                  <th>Step</th>
                  <th>Error</th>
                  <th>Failed</th>
                </tr>
              </thead>
              <tbody>
                {(stats.repeat_failures ?? []).map((r) => (
                  <tr key={r.job_id}>
                    <td>
                      {r.series} Ep {r.episode}
                    </td>
                    <td>
                      <span className={`badge ${r.attempts >= 3 ? 'blue' : 'gray'}`}>{r.attempts}</span>
                    </td>
                    <td>{r.node_name || '—'}</td>
                    <td className="muted">{r.step || '—'}</td>
                    <td className="muted" title={r.error} style={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {r.error || '—'}
                    </td>
                    <td className="muted">{r.finished_at || '—'}</td>
                  </tr>
                ))}
                {(stats.repeat_failures ?? []).length === 0 && (
                  <tr>
                    <td colSpan={6} className="muted">
                      No episodes failed more than once in this range.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* Per-node breakdown: name, done, failed, avg duration. */}
          <div className="card">
            <h3 style={{ marginTop: 0 }}>Per node</h3>
            <table>
              <thead>
                <tr>
                  <th>Node</th>
                  <th>Done</th>
                  <th>Failed</th>
                  <th>Avg duration</th>
                  <th>Speedup</th>
                </tr>
              </thead>
              <tbody>
                {stats.per_node.map((r) => (
                  <tr key={r.node_id}>
                    <td>{r.name}</td>
                    <td>{r.done}</td>
                    <td>{r.failed}</td>
                    <td className="muted">{humanizeSeconds(r.avg_duration_sec)}</td>
                    <td className="muted" title="Media seconds per encode second">{formatSpeedup(r.avg_speedup)}</td>
                  </tr>
                ))}
                {stats.per_node.length === 0 && (
                  <tr>
                    <td colSpan={5} className="muted">
                      No finished jobs by node in this range.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* Per-flow breakdown: name, done, failed, avg duration. */}
          <div className="card">
            <h3 style={{ marginTop: 0 }}>Per flow</h3>
            <table>
              <thead>
                <tr>
                  <th>Flow</th>
                  <th>Done</th>
                  <th>Failed</th>
                  <th>Avg duration</th>
                  <th>Speedup</th>
                </tr>
              </thead>
              <tbody>
                {stats.per_flow.map((r) => (
                  <tr key={r.flow_id}>
                    <td>{r.name}</td>
                    <td>{r.done}</td>
                    <td>{r.failed}</td>
                    <td className="muted">{humanizeSeconds(r.avg_duration_sec)}</td>
                    <td className="muted" title="Media seconds per encode second">{formatSpeedup(r.avg_speedup)}</td>
                  </tr>
                ))}
                {stats.per_flow.length === 0 && (
                  <tr>
                    <td colSpan={5} className="muted">
                      No finished jobs by flow in this range.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* Failures by step: where jobs died, ordered by count DESC. */}
          <div className="card">
            <h3 style={{ marginTop: 0 }}>Failures by step</h3>
            <table>
              <thead>
                <tr>
                  <th>Step</th>
                  <th>Failures</th>
                </tr>
              </thead>
              <tbody>
                {stats.failures_by_step.map((r, i) => (
                  <tr key={`${r.step}-${i}`}>
                    <td>{r.step || '—'}</td>
                    <td>{r.count}</td>
                  </tr>
                ))}
                {stats.failures_by_step.length === 0 && (
                  <tr>
                    <td colSpan={2} className="muted">
                      No failures in this range.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* Per-day throughput: CSS bars (no chart lib). Each bucket is one
              day's done count; bar width scales to the max bucket in range. */}
          <div className="card">
            <h3 style={{ marginTop: 0 }}>Done per day</h3>
            <DayBars days={stats.per_day} />
          </div>
        </>
      )}
    </>
  );
}

// Stat is a single label/value card. Mirrors the Dashboard Stat component's
// styling (badge tones) so the summary strip reads consistently. Kept local
// rather than shared because Dashboard's is local too — YAGNI to extract.
function Stat({
  label,
  value,
  tone,
  title,
}: {
  label: string;
  value: string | number;
  tone?: 'blue' | 'green' | 'gray';
  title?: string;
}) {
  const toneCls = tone ?? 'gray';
  return (
    <div title={title} style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <span className="muted" style={{ fontSize: 11, textTransform: 'uppercase' }}>
        {label}
      </span>
      <span className={`badge ${toneCls}`} style={{ fontSize: 16, alignSelf: 'flex-start' }}>
        {value}
      </span>
    </div>
  );
}

// DayBars renders the per-day done-throughput trend as simple CSS bars. No
// chart library — the range is at most 30 buckets (30d) or unbounded for
// "all", but "all" on a real fleet is still a bounded day count. Bar width
// scales relative to the max bucket so a single busy day is visibly taller.
function DayBars({ days }: { days: { date: string; count: number }[] }) {
  if (days.length === 0) {
    return <p className="muted">No completed jobs in this range.</p>;
  }
  const max = Math.max(1, ...days.map((d) => d.count));
  return (
    <div style={{ display: 'flex', alignItems: 'flex-end', gap: 4, height: 120, flexWrap: 'wrap' }}>
      {days.map((d) => (
        <div
          key={d.date}
          title={`${d.date}: ${d.count}`}
          style={{
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            gap: 2,
            minWidth: 28,
          }}
        >
          <div
            style={{
              width: 20,
              height: Math.max(2, (d.count / max) * 100),
              background: 'var(--accent, #4a9)',
              borderRadius: 3,
            }}
          />
          <span className="muted" style={{ fontSize: 9, transform: 'rotate(-45deg)', whiteSpace: 'nowrap' }}>
            {d.date.slice(5)}
          </span>
        </div>
      ))}
    </div>
  );
}

// humanizeSeconds turns a seconds value into a compact "1h 23m" / "45m" /
// "30s" / "—" string. Used for avg-duration cells. 0 → "—".
function humanizeSeconds(sec: number): string {
  // Non-finite (NaN/Infinity) or non-positive durations render a placeholder,
  // never garbage like "Infinityh NaNm" — same discipline as
  // StepTimingsView.humanizeDuration. 0 → "—" (no meaningful duration).
  if (!Number.isFinite(sec) || sec <= 0) return '—';
  const s = Math.round(sec);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rs = s % 60;
  if (m < 60) return rs ? `${m}m ${rs}s` : `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return rm ? `${h}h ${rm}m` : `${h}h`;
}

// formatSpeedup renders an encode-speedup ratio as "2.3×". The backend
// reports 0 when no job in range carried a usable duration_sec metric —
// that is "no data", never "0.0×". Sub-0.1 ratios (very slow 4K encodes)
// get two decimals so a real slow node never displays as "0.0×".
function formatSpeedup(x: number): string {
  if (!Number.isFinite(x) || x <= 0) return '—';
  if (x < 0.1) return `${x.toFixed(2)}×`;
  return `${x.toFixed(x >= 10 ? 0 : 1)}×`;
}
