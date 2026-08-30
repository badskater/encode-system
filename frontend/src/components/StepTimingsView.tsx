import type { StepTiming } from '../types';

// humanizeDuration turns seconds into a compact label:
//   < 60s  → "Ns"
//   < 1h   → "Mm"  (or "Mm Ss" when seconds remain and minutes < 10)
//   >= 1h  → "Hh MMm"
// No chart library — the breakdown is a handful of rows, YAGNI.
function humanizeDuration(sec: number): string {
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return `${s}s`;
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const rem = s % 60;
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`;
  // Sub-hour: show seconds too when the value is short enough to matter.
  if (m < 10 && rem > 0) return `${m}m ${rem}s`;
  return `${m}m`;
}

// StepTimingsView renders the per-step timing breakdown from the agent's
// completion report. The parent decides whether to render it (empty array
// → renders nothing).
export default function StepTimingsView({ timings }: { timings: StepTiming[] }) {
  if (!timings || timings.length === 0) return null;
  const maxDur = Math.max(...timings.map((t) => t.duration_sec), 1);
  return (
    <>
      <h4>Step timings</h4>
      <table>
        <thead>
          <tr>
            <th>Step</th>
            <th>Started</th>
            <th>Duration</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {timings.map((t, i) => {
            const pct = Math.max(2, Math.round((t.duration_sec / maxDur) * 100));
            return (
              <tr key={`${t.step}-${i}`}>
                <td>{t.step}</td>
                <td className="muted">{fmtStarted(t.started_at)}</td>
                <td>{humanizeDuration(t.duration_sec)}</td>
                <td>
                  <div className="step-bar-track">
                    <div className="step-bar-fill" style={{ width: `${pct}%` }} />
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

// fmtStarted normalizes backend timestamps (which may arrive without a Z)
// and renders a locale string, mirroring helpers.fmtTime but local to keep
// this view self-contained for the timing table.
function fmtStarted(iso: string): string {
  const d = new Date(iso.endsWith('Z') || iso.includes('T') ? iso : iso + 'Z');
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}
