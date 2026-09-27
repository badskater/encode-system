import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import NodeMetricChips from './NodeMetricChips';
import type { NodeMetrics } from '../types';

function metrics(overrides: Partial<NodeMetrics> = {}): NodeMetrics {
  return {
    cpu_pct: 42,
    mem_used_mb: 8192,
    mem_total_mb: 16384,
    disk_free_gb: 500,
    gpu_util: 55,
    gpu_temp: 60,
    gpu_mem_used_mb: 2048,
    encode_fps: 24.5,
    ...overrides,
  };
}

describe('NodeMetricChips', () => {
  it('renders the CPU percentage value', () => {
    render(<NodeMetricChips metrics={metrics({ cpu_pct: 73 })} />);
    expect(screen.getByText(/73/)).toBeInTheDocument();
  });

  it('applies a warn color class when CPU > 80%', () => {
    const { container } = render(
      <NodeMetricChips metrics={metrics({ cpu_pct: 92 })} />,
    );
    const cpuChip = container.querySelector('[data-chip="cpu"]')!;
    expect(cpuChip.className).toMatch(/warn/i);
  });

  it('does not apply the warn class when CPU <= 80%', () => {
    const { container } = render(
      <NodeMetricChips metrics={metrics({ cpu_pct: 50 })} />,
    );
    const cpuChip = container.querySelector('[data-chip="cpu"]')!;
    expect(cpuChip.className).not.toMatch(/warn/i);
  });

  it('hides GPU chips when gpu_util === -1 (no GPU)', () => {
    render(
      <NodeMetricChips metrics={metrics({ gpu_util: -1, gpu_temp: -1, gpu_mem_used_mb: -1 })} />,
    );
    expect(containerNoGpu('gpu-util')).toBe(false);
    expect(containerNoGpu('gpu-temp')).toBe(false);
    expect(containerNoGpu('gpu-mem')).toBe(false);
    // Helper: query the DOM rendered by the component above.
    function containerNoGpu(chip: string): boolean {
      return !!document.querySelector(`[data-chip="${chip}"]`);
    }
  });

  it('hides the encode-fps chip when fps is 0 (idle / not encoding)', () => {
    render(<NodeMetricChips metrics={metrics({ encode_fps: 0 })} />);
    expect(document.querySelector('[data-chip="fps"]')).toBeNull();
  });

  it('shows the encode-fps chip when fps > 0', () => {
    render(<NodeMetricChips metrics={metrics({ encode_fps: 30 })} />);
    expect(document.querySelector('[data-chip="fps"]')).not.toBeNull();
  });

  it('renders RAM used / total in GB', () => {
    render(<NodeMetricChips metrics={metrics({ mem_used_mb: 8192, mem_total_mb: 16384 })} />);
    expect(screen.getByText(/8\.0/)).toBeInTheDocument(); // 8 GB used
    expect(screen.getByText(/16\.0/)).toBeInTheDocument(); // 16 GB total
  });

  it('renders disk free in GB', () => {
    render(<NodeMetricChips metrics={metrics({ disk_free_gb: 123.4 })} />);
    expect(screen.getByText(/123/)).toBeInTheDocument();
  });

  it('renders nothing when metrics is undefined (no empty chips)', () => {
    const { container } = render(<NodeMetricChips metrics={undefined} />);
    expect(container.querySelector('[data-chip]')).toBeNull();
  });

  it('applies the warn class to the disk chip when free space is below the alert threshold', () => {
    const { container } = render(
      <NodeMetricChips metrics={metrics({ disk_free_gb: 20 })} diskAlertGB={50} />,
    );
    const diskChip = container.querySelector('[data-chip="disk"]')!;
    expect(diskChip.className).toMatch(/warn/i);
  });

  it('does not warn when disk is above the threshold', () => {
    const { container } = render(
      <NodeMetricChips metrics={metrics({ disk_free_gb: 200 })} diskAlertGB={50} />,
    );
    const diskChip = container.querySelector('[data-chip="disk"]')!;
    expect(diskChip.className).not.toMatch(/warn/i);
  });

  it('does not warn when the threshold is 0 (disabled), even at 1 GB free', () => {
    const { container } = render(
      <NodeMetricChips metrics={metrics({ disk_free_gb: 1 })} diskAlertGB={0} />,
    );
    const diskChip = container.querySelector('[data-chip="disk"]')!;
    expect(diskChip.className).not.toMatch(/warn/i);
  });
});
