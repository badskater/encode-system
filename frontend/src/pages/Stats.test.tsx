import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import StatsPage from './Stats';
import { api } from '../api/client';
import type { Stats } from '../types';

// Stats page tests (Phase E). The page fetches on mount via
// api.getStats(range) and re-fetches when the range changes. These tests
// mock the api method (not fetch) to drive renders without real timers.

// A full, populated Stats fixture — matches the backend's json tags exactly
// (range_days=7, totals + one row per breakdown).
function fullStats(overrides: Partial<Stats> = {}): Stats {
  return {
    range_days: 7,
    totals: { done: 10, failed: 2, cancelled: 1, avg_duration_sec: 3600, avg_speedup: 2.5 },
    per_node: [
      { node_id: 1, name: 'enc-01', done: 6, failed: 1, avg_duration_sec: 1800, avg_speedup: 3.1 },
      { node_id: 2, name: 'enc-02', done: 4, failed: 1, avg_duration_sec: 5400, avg_speedup: 0 },
    ],
    per_flow: [
      { flow_id: 1, name: 'hevc', done: 8, failed: 1, avg_duration_sec: 3000, avg_speedup: 2.2 },
      { flow_id: 2, name: 'av1', done: 2, failed: 1, avg_duration_sec: 7200, avg_speedup: 12.4 },
    ],
    failures_by_step: [
      { step: 'encode', count: 1 },
      { step: 'mux', count: 1 },
    ],
    per_day: [
      { date: '2026-08-29', count: 4 },
      { date: '2026-08-30', count: 6 },
    ],
    repeat_failures: [
      {
        job_id: 42, series: 'Some Show', episode: '07', node_id: 9, node_name: 'enc-77',
        step: 'dgindex', attempts: 5, error: 'x265 crashed', finished_at: '2026-08-30 12:00:00',
      },
    ],
    ...overrides,
  };
}

// The zeroed shape the backend returns for a fresh/empty fleet — every
// array empty, every count 0, every avg 0. The page must render cleanly.
function emptyStats(): Stats {
  return {
    range_days: 7,
    totals: { done: 0, failed: 0, cancelled: 0, avg_duration_sec: 0, avg_speedup: 0 },
    per_node: [],
    per_flow: [],
    failures_by_step: [],
    per_day: [],
    repeat_failures: [],
  };
}

describe('Stats page', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the totals summary cards from mocked getStats', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(fullStats());

    render(<StatsPage />);

    // The default range is 7d.
    await waitFor(() => expect(api.getStats).toHaveBeenCalledWith('7d'));

    // Totals render as badge values. Use getAllByText because counts like
    // "2" also appear in per-node/per-flow tables (failed columns).
    await screen.findByText('10'); // done (unique)
    expect(screen.getAllByText('1').length).toBeGreaterThanOrEqual(1); // cancelled
    // 3600s → humanizeSeconds → "1h" (exact hour, no trailing 0m)
    expect(screen.getByText('1h')).toBeInTheDocument();
  });

  it('renders per-node, per-flow, and failures-by-step rows', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(fullStats());

    render(<StatsPage />);

    await screen.findByText('enc-01');
    expect(screen.getByText('enc-02')).toBeInTheDocument();
    expect(screen.getByText('hevc')).toBeInTheDocument();
    expect(screen.getByText('av1')).toBeInTheDocument();
    // Failures-by-step shows the step name and count.
    expect(screen.getByText('encode')).toBeInTheDocument();
    expect(screen.getByText('mux')).toBeInTheDocument();
  });

  it('re-fetches with the new range when the range button is clicked', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(fullStats());

    render(<StatsPage />);

    // Initial fetch is 7d (default).
    await waitFor(() => expect(api.getStats).toHaveBeenCalledWith('7d'));
    expect(api.getStats).toHaveBeenCalledTimes(1);

    // Click the 24h range button.
    fireEvent.click(screen.getByRole('button', { name: '24h' }));

    await waitFor(() => expect(api.getStats).toHaveBeenCalledWith('24h'));
    // Two calls now: 7d on mount, 24h on range change.
    expect(api.getStats).toHaveBeenCalledTimes(2);
  });

  it('renders the empty/zeroed stats shape without crashing', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(emptyStats());

    render(<StatsPage />);

    // Loading state clears and the empty-placeholders render.
    await screen.findByText(/No finished jobs by node/i);
    expect(screen.getByText(/No finished jobs by flow/i)).toBeInTheDocument();
    expect(screen.getByText(/No failures in this range/i)).toBeInTheDocument();
    expect(screen.getByText(/No completed jobs in this range/i)).toBeInTheDocument();
  });
});

describe('Stats page — speedup + stuck episodes', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the avg speedup card and per-node/per-flow speedup columns', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(fullStats());

    render(<StatsPage />);

    // Totals card: 2.5 -> "2.5×".
    await screen.findByText('Avg speedup');
    expect(screen.getByText('2.5×')).toBeInTheDocument();
    // Per-node: 3.1 -> "3.1×"; a 0 row renders the no-data placeholder.
    expect(screen.getByText('3.1×')).toBeInTheDocument();
    // Per-flow: 12.4 rounds to a whole number at >=10 -> "12×".
    expect(screen.getByText('12×')).toBeInTheDocument();
    expect(screen.getByText('2.2×')).toBeInTheDocument();
    // No-data cells render an em dash, never "0.0×".
    const dashes = screen.getAllByText('—');
    expect(dashes.length).toBeGreaterThan(0);
    expect(screen.queryByText('0.0×')).not.toBeInTheDocument();
  });

  it('renders the stuck-episodes card with attempts and error text', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(fullStats());

    render(<StatsPage />);

    await screen.findByText(/Stuck episodes/i);
    expect(screen.getByText('Some Show Ep 07')).toBeInTheDocument();
    // Attempts badge shows retry_count+1 (5).
    expect(screen.getByText('5')).toBeInTheDocument();
    expect(screen.getByText('x265 crashed')).toBeInTheDocument();
    expect(screen.getByText('enc-77')).toBeInTheDocument();
  });

  it('shows the empty-state row when no episode failed repeatedly', async () => {
    vi.spyOn(api, 'getStats').mockResolvedValue(emptyStats());

    render(<StatsPage />);

    await screen.findByText(/No episodes failed more than once/i);
  });
});
