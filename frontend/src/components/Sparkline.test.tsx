import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import Sparkline from './Sparkline';

describe('Sparkline', () => {
  it('renders an svg polyline for 3+ values', () => {
    const { container } = render(<Sparkline values={[10, 50, 90]} />);
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    const poly = svg!.querySelector('polyline');
    expect(poly).not.toBeNull();
    const points = poly!.getAttribute('points') ?? '';
    expect(points.split(' ').filter(Boolean)).toHaveLength(3);
  });

  it('scales points to the svg width/height with min/max padding', () => {
    const { container } = render(<Sparkline values={[0, 100]} width={100} height={40} />);
    const poly = container.querySelector('svg polyline')!;
    const pts = (poly.getAttribute('points') ?? '').split(' ').filter(Boolean);
    expect(pts).toHaveLength(2);
    // First point at x=0 (left edge), last at x=100 (right edge).
    const [x0] = pts[0].split(',');
    const [x1] = pts[1].split(',');
    expect(Number(x0)).toBeCloseTo(0, 1);
    expect(Number(x1)).toBeCloseTo(100, 1);
    // y values must be within [0, height] — no NaN.
    for (const pt of pts) {
      const [, y] = pt.split(',');
      const yn = Number(y);
      expect(Number.isFinite(yn)).toBe(true);
      expect(yn).toBeGreaterThanOrEqual(0);
      expect(yn).toBeLessThanOrEqual(40);
    }
  });

  it('sanitizes non-finite values — no NaN in the points attribute', () => {
    const { container } = render(
      <Sparkline values={[10, Number.NaN, Number.POSITIVE_INFINITY, 90]} />,
    );
    const poly = container.querySelector('svg polyline')!;
    const points = poly.getAttribute('points') ?? '';
    expect(points).not.toContain('NaN');
    expect(points).not.toContain('Infinity');
    // Every coordinate must parse to a finite number.
    for (const pt of points.split(' ').filter(Boolean)) {
      const [x, y] = pt.split(',');
      expect(Number.isFinite(Number(x))).toBe(true);
      expect(Number.isFinite(Number(y))).toBe(true);
    }
  });

  it('renders a flat baseline for a single value (no NaN, no crash)', () => {
    const { container } = render(<Sparkline values={[42]} />);
    const poly = container.querySelector('svg polyline');
    // Single value → flat baseline (one point or a flat line spanning the
    // width). Either way: no NaN, component does not throw.
    if (poly) {
      const points = poly.getAttribute('points') ?? '';
      expect(points).not.toContain('NaN');
    }
  });

  it('renders nothing (or an empty svg) for an empty array — no crash', () => {
    const { container } = render(<Sparkline values={[]} />);
    const poly = container.querySelector('svg polyline');
    // No polyline, or a polyline with empty points — but never a crash.
    if (poly) {
      const points = poly.getAttribute('points') ?? '';
      expect(points).not.toContain('NaN');
    }
  });

  it('renders a label when provided', () => {
    const { container } = render(<Sparkline values={[10, 20, 30]} label="CPU" />);
    expect(container.textContent).toContain('CPU');
  });

  it('renders the unit in the label when provided', () => {
    const { container } = render(
      <Sparkline values={[10, 20, 30]} label="CPU" unit="%" />,
    );
    expect(container.textContent).toContain('%');
  });
});
