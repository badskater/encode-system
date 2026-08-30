import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import NodeMetricsPanel from './NodeMetricsPanel';
import { api } from '../api/client';
import type { NodeMetricSample } from '../types';

function sample(overrides: Partial<NodeMetricSample> = {}): NodeMetricSample {
  return {
    ts: '2026-08-30 07:00:00',
    cpu_pct: 40,
    mem_used_mb: 4096,
    mem_total_mb: 16384,
    disk_free_gb: 200,
    gpu_util: 50,
    gpu_temp: 65,
    gpu_mem_used_mb: 1024,
    encode_fps: 23.5,
    ...overrides,
  };
}

describe('NodeMetricsPanel', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('fetches metrics on mount and renders CPU + GPU-util sparklines', async () => {
    vi.spyOn(api, 'getNodeMetrics').mockResolvedValue([
      sample({ cpu_pct: 10 }),
      sample({ cpu_pct: 50 }),
      sample({ cpu_pct: 90, gpu_util: 80 }),
    ]);
    render(<NodeMetricsPanel nodeId={5} nodeName="enc-05" onClose={() => {}} />);

    await waitFor(() => expect(api.getNodeMetrics).toHaveBeenCalledWith(5, '1h'));
    await waitFor(() => expect(screen.getByText(/CPU/i)).toBeInTheDocument());
    expect(screen.getByText(/GPU/i)).toBeInTheDocument();
  });

  it('shows the latest encode fps from the samples', async () => {
    vi.spyOn(api, 'getNodeMetrics').mockResolvedValue([
      sample({ encode_fps: 10 }),
      sample({ encode_fps: 24.7 }),
    ]);
    render(<NodeMetricsPanel nodeId={1} nodeName="n1" onClose={() => {}} />);
    await waitFor(() => expect(screen.getByText(/24\.7/)).toBeInTheDocument());
  });

  it('re-fetches when the range switches from 1h to 6h', async () => {
    const spy = vi
      .spyOn(api, 'getNodeMetrics')
      .mockResolvedValue([sample()]);
    render(<NodeMetricsPanel nodeId={2} nodeName="n2" onClose={() => {}} />);
    await waitFor(() => expect(spy).toHaveBeenCalledWith(2, '1h'));

    fireEvent.click(screen.getByRole('button', { name: /6h/i }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith(2, '6h'));
  });

  it('re-fetches when the range switches from 1h to 24h', async () => {
    const spy = vi
      .spyOn(api, 'getNodeMetrics')
      .mockResolvedValue([sample()]);
    render(<NodeMetricsPanel nodeId={3} nodeName="n3" onClose={() => {}} />);
    await waitFor(() => expect(spy).toHaveBeenCalledWith(3, '1h'));

    fireEvent.click(screen.getByRole('button', { name: /24h/i }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith(3, '24h'));
  });

  it('shows an error message when the fetch rejects', async () => {
    vi.spyOn(api, 'getNodeMetrics').mockRejectedValue(
      new Error('503: metrics store unavailable'),
    );
    render(<NodeMetricsPanel nodeId={7} nodeName="n7" onClose={() => {}} />);
    await waitFor(() =>
      expect(screen.getByText(/metrics store unavailable/)).toBeInTheDocument(),
    );
  });

  it('shows a loading indicator before the first fetch resolves', () => {
    vi.spyOn(api, 'getNodeMetrics').mockReturnValue(
      new Promise<NodeMetricSample[]>(() => {}),
    );
    render(<NodeMetricsPanel nodeId={9} nodeName="n9" onClose={() => {}} />);
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
  });
});
