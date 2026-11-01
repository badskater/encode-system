import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, it, vi, beforeEach, expect } from 'vitest';
import { RoleContext } from '../hooks/useRole';
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


describe('Dashboard scan now', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(settings);
    vi.spyOn(api, 'jobs').mockResolvedValue([]);
  });

  it('renders a Scan now button for admins', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    render(<RoleContext.Provider value="admin"><Dashboard /></RoleContext.Provider>);
    await waitFor(() => expect(screen.getByRole('button', { name: /scan now/i })).toBeInTheDocument());
  });

  it('hides the Scan now button for viewers', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    render(<RoleContext.Provider value="viewer"><Dashboard /></RoleContext.Provider>);
    await waitFor(() => expect(screen.getByText(/Dashboard/)).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: /scan now/i })).not.toBeInTheDocument();
  });

  it('calls scanNow and shows Scan triggered on click', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    const scanSpy = vi.spyOn(api, 'scanNow').mockResolvedValue({ triggered: true });
    render(<RoleContext.Provider value="admin"><Dashboard /></RoleContext.Provider>);
    
    const btn = await screen.findByRole('button', { name: /scan now/i });
    fireEvent.click(btn);
    await waitFor(() => expect(scanSpy).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText(/scan triggered/i)).toBeInTheDocument());
  });

  it('disables the button while scanning', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    // Make scanNow hang so we can observe the disabled state
    vi.spyOn(api, 'scanNow').mockReturnValue(new Promise(() => {}));
    render(<RoleContext.Provider value="admin"><Dashboard /></RoleContext.Provider>);
    
    const btn = await screen.findByRole('button', { name: /scan now/i });
    fireEvent.click(btn);
    await waitFor(() => expect((btn as HTMLButtonElement).disabled).toBe(true));
    expect(screen.getByText(/scanning/i)).toBeInTheDocument();
  });
});
