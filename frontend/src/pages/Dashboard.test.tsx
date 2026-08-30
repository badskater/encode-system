import { render, screen } from '@testing-library/react';
import { describe, it, vi, beforeEach } from 'vitest';
import Dashboard from './Dashboard';
import { api } from '../api/client';
import type { Node, Settings } from '../types';

function node(overrides: Partial<Node> = {}): Node {
  return {
    id: 1,
    name: 'enc-01',
    enabled: true,
    status: 'idle',
    agent_version: '1.0',
    lib_version: 1,
    tasks_since_boot: 0,
    reboot_pending: false,
    last_seen: null,
    online: true,
    ...overrides,
  };
}

// Settings has two interface declarations in types.ts (legacy + newer);
// tsc requires the union of all fields. Provide every field so the literal
// satisfies the merged interface regardless of declaration order.
const settings: Settings = {
  group: 'g',
  tag: 't',
  tasks_before_reboot: 10,
  default_flow: '',
  scripts_root: '',
  release_root: '',
  scripts_share: '',
  release_share: '',
  node_bin_dir: '',
  node_scripts_dir: '',
  node_release_dir: '',
  scan_interval_seconds: 5,
  discord_webhook: '',
};

describe('Dashboard fleet strip', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    // Settings poll() resolves immediately; stub a benign response so the
    // component doesn't sit on a pending promise.
    vi.spyOn(api, 'settings').mockResolvedValue(settings);
    vi.spyOn(api, 'jobs').mockResolvedValue([]);
  });

  it('excludes nodes without last_metrics from the avg fps computation', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([
      node({ id: 1, name: 'a', last_metrics: { cpu_pct: 10, mem_used_mb: 0, mem_total_mb: 0, disk_free_gb: 0, gpu_util: 50, gpu_temp: 60, gpu_mem_used_mb: 0, encode_fps: 24 } }),
      node({ id: 2, name: 'b', last_metrics: { cpu_pct: 20, mem_used_mb: 0, mem_total_mb: 0, disk_free_gb: 0, gpu_util: 70, gpu_temp: 65, gpu_mem_used_mb: 0, encode_fps: 36 } }),
      node({ id: 3, name: 'c-no-metrics' }), // no last_metrics → excluded
    ]);
    render(<Dashboard />);
    // avg fps over only a+b = (24+36)/2 = 30
    await waitForText('30.0');
  });

  it('excludes nodes without last_metrics from the avg GPU util', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([
      node({ id: 1, name: 'a', last_metrics: { cpu_pct: 0, mem_used_mb: 0, mem_total_mb: 0, disk_free_gb: 0, gpu_util: 40, gpu_temp: 60, gpu_mem_used_mb: 0, encode_fps: 10 } }),
      node({ id: 2, name: 'b', last_metrics: { cpu_pct: 0, mem_used_mb: 0, mem_total_mb: 0, disk_free_gb: 0, gpu_util: 60, gpu_temp: 65, gpu_mem_used_mb: 0, encode_fps: 10 } }),
      node({ id: 3, name: 'c' }), // excluded
    ]);
    render(<Dashboard />);
    // avg gpu util over only a+b = (40+60)/2 = 50
    await waitForText(/50/);
  });

  it('excludes nodes whose gpu_util is -1 (no GPU) from avg GPU util', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([
      node({ id: 1, name: 'gpu', last_metrics: { cpu_pct: 0, mem_used_mb: 0, mem_total_mb: 0, disk_free_gb: 0, gpu_util: 80, gpu_temp: 70, gpu_mem_used_mb: 0, encode_fps: 10 } }),
      node({ id: 2, name: 'nogpu', last_metrics: { cpu_pct: 0, mem_used_mb: 0, mem_total_mb: 0, disk_free_gb: 0, gpu_util: -1, gpu_temp: -1, gpu_mem_used_mb: -1, encode_fps: 0 } }),
    ]);
    render(<Dashboard />);
    // Only the GPU node counts → avg gpu util = 80
    await waitForText(/80/);
  });

  it('shows a placeholder when no nodes have metrics yet', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([
      node({ id: 1, name: 'bare' }), // no last_metrics
    ]);
    render(<Dashboard />);
    await waitForText(/no metrics/i);
  });
});

// helper: wait for some text on the rendered dashboard, with a timeout.
function waitForText(match: string | RegExp) {
  return screen.findByText(
    typeof match === 'string' ? new RegExp(escapeRegex(match)) : match,
    {},
    { timeout: 2000 },
  );
}

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
