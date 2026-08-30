// Sparkline: a tiny inline SVG line chart with no chart-library dependency.
// Used for CPU% and GPU-util history in the node metrics panel.
//
// Sanitization discipline (same as StepTimingsView): non-finite values
// (NaN/Infinity from a corrupt or missing sample) are coerced to 0 BEFORE
// computing min/max, so a single bad value can never poison the whole
// scale and turn every coordinate into NaN. Empty arrays render nothing;
// single-value arrays render a flat baseline at the value's height.
export default function Sparkline({
  values,
  width = 120,
  height = 32,
  label,
  unit,
}: {
  values: number[];
  width?: number;
  height?: number;
  label?: string;
  unit?: string;
}) {
  // Sanitize: coerce non-finite to 0. Done once, up front, so downstream
  // math is always over clean numbers.
  const clean: number[] = (values ?? []).map((v) =>
    Number.isFinite(v) ? v : 0,
  );

  // Empty array → render the label (if any) but no chart geometry.
  if (clean.length === 0) {
    return label ? (
      <span className="sparkline-wrap">
        <span className="muted sparkline-label">{label}{unit ?? ''}</span>
      </span>
    ) : null;
  }

  // Min/max with padding so a flat series doesn't collapse to a zero-height
  // line. When all values are equal, span is 0 → use a small visual span so
  // the line sits at the vertical midpoint rather than on an edge.
  const min = Math.min(...clean);
  const max = Math.max(...clean);
  const span = max - min;
  // Pad the range by 10% of the span (or a unit fallback when flat) so the
  // line doesn't touch the top/bottom edges.
  const pad = span === 0 ? 1 : span * 0.1;
  const lo = min - pad;
  const hi = max + pad;
  const range = hi - lo || 1; // guard against div-by-zero

  // Map a value to an (x, y) pair in svg coords. x is evenly spaced across
  // [0, width]; y is inverted (svg origin is top-left) and clamped to
  // [0, height].
  const n = clean.length;
  const pts = clean.map((v, i) => {
    const x = n === 1 ? width / 2 : (i / (n - 1)) * width;
    const yRaw = height - ((v - lo) / range) * height;
    const y = Math.max(0, Math.min(height, yRaw));
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });

  const pointsStr = pts.join(' ');

  return (
    <span className="sparkline-wrap" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      {label && (
        <span className="muted sparkline-label">{label}{unit ?? ''}</span>
      )}
      <svg
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={label ? `${label}${unit ?? ''}` : 'sparkline'}
        style={{ display: 'block', verticalAlign: 'middle' }}
      >
        <polyline
          points={pointsStr}
          fill="none"
          stroke="currentColor"
          strokeWidth={1.5}
          strokeLinejoin="round"
          strokeLinecap="round"
        />
      </svg>
    </span>
  );
}
