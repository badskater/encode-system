import type { NodeMetrics } from '../types';

// NodeMetricChips renders a compact inline row of the live counters an
// encode agent reports each heartbeat. Chips only appear when the backend
// sent a real value; absent metrics → nothing rendered (no empty chips).
//
// GPU chips are gated on gpu_util >= 0: the agent reports -1 when the host
// has no GPU, so we suppress util/temp/mem entirely in that case. The
// encode-fps chip is hidden when 0 (idle / not currently encoding).
//
// The CPU chip gets a "warn" class when usage exceeds 80% — the same visual
// hint as the dashboard's "reboot approaching" marker.
export default function NodeMetricChips({ metrics }: { metrics?: NodeMetrics }) {
  if (!metrics) return null;
  const m = metrics;

  const cpuWarn = m.cpu_pct > 80;

  // GB helpers — MB → GB with one decimal, matching the dashboard style.
  const toGB = (mb: number) => (mb / 1024).toFixed(1);
  const diskGB = (gb: number) => gb.toFixed(1);

  const hasGpu = m.gpu_util >= 0;
  const encoding = m.encode_fps > 0;

  return (
    <span className="metric-chips">
      <span className={`chip ${cpuWarn ? 'warn' : ''}`} data-chip="cpu" title="CPU utilization">
        <span className="chip-label">CPU</span>
        <span className="chip-value">{fmtPct(m.cpu_pct)}</span>
      </span>
      <span className="chip" data-chip="ram" title="Memory used / total">
        <span className="chip-label">RAM</span>
        <span className="chip-value">{toGB(m.mem_used_mb)}/{toGB(m.mem_total_mb)}GB</span>
      </span>
      <span className="chip" data-chip="disk" title="Free disk space">
        <span className="chip-label">DISK</span>
        <span className="chip-value">{diskGB(m.disk_free_gb)}GB</span>
      </span>
      {hasGpu && (
        <>
          <span className="chip" data-chip="gpu-util" title="GPU utilization">
            <span className="chip-label">GPU</span>
            <span className="chip-value">{fmtPct(m.gpu_util)}</span>
          </span>
          <span className="chip" data-chip="gpu-temp" title="GPU temperature">
            <span className="chip-label">TEMP</span>
            <span className="chip-value">{Math.round(m.gpu_temp)}°C</span>
          </span>
          <span className="chip" data-chip="gpu-mem" title="GPU memory used / total">
            <span className="chip-label">VRAM</span>
            <span className="chip-value">{toGB(m.gpu_mem_used_mb)}GB</span>
          </span>
        </>
      )}
      {encoding && (
        <span className="chip" data-chip="fps" title="Encode frames per second">
          <span className="chip-label">FPS</span>
          <span className="chip-value">{fmtFps(m.encode_fps)}</span>
        </span>
      )}
    </span>
  );
}

// fmtPct renders a percentage with no trailing decimals (92 → "92%").
// Non-finite values fall back to "—" rather than "NaN%".
function fmtPct(v: number): string {
  if (!Number.isFinite(v)) return '—';
  return `${Math.round(v)}%`;
}

// fmtFps renders encode fps with one decimal (24.5 → "24.5 fps").
function fmtFps(v: number): string {
  if (!Number.isFinite(v)) return '—';
  return `${v.toFixed(1)} fps`;
}
