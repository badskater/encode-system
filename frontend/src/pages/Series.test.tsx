import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import SeriesPage from './Series';
import { api } from '../api/client';
import type { Series } from '../types';

// usePolling resolves fn() immediately on mount; mocking the api methods is
// enough to drive the page render without waiting for real timers.
function seriesFixture(overrides: Partial<Series> = {}): Series {
  return {
    id: 1,
    name: 'Show A',
    flow_id: 0,
    tag: '',
    enabled: true,
    jobs: 0,
    created_at: '2026-01-02T03:04:05Z',
    updated_at: '2026-01-02T03:04:05Z',
    ...overrides,
  };
}

function mockEmpty() {
  vi.spyOn(api, 'flows').mockResolvedValue([]);
  // api.settings is typed as the merged Settings interface; provide a full
  // fixture so TypeScript is satisfied across both declared shapes.
  vi.spyOn(api, 'settings').mockResolvedValue({
    controller_url: '',
    nfs_server: '',
    scripts_share: '',
    release_share: '',
    scripts_root: '',
    release_root: '',
    node_bin_dir: '',
    node_scripts_dir: '',
    node_release_dir: '',
    scan_interval_seconds: 0,
    tasks_before_reboot: 0,
    group: '',
    tag: 'default',
    discord_webhook: '',
    default_flow: '',
  });
}

describe('Series page progress column', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the Progress column header', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    expect(screen.getByText('Progress')).toBeInTheDocument();
  });

  it('renders "done/total" counts from mock data', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_done: 12, episodes_total: 24 }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('12/24')).toBeInTheDocument());

    expect(screen.getByText('12/24')).toBeInTheDocument();
  });

  it('scales the bar width to done/total %', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_done: 12, episodes_total: 24 }),
    ]);
    mockEmpty();

    const { container } = render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('12/24')).toBeInTheDocument());

    const bar = container.querySelector<HTMLElement>('.step-bar-fill')!;
    // 12/24 = 50%
    expect(bar.style.width).toBe('50%');
  });

  it('clamps the bar to 100% when done exceeds total', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_done: 30, episodes_total: 24 }),
    ]);
    mockEmpty();

    const { container } = render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('30/24')).toBeInTheDocument());

    const bar = container.querySelector<HTMLElement>('.step-bar-fill')!;
    expect(bar.style.width).toBe('100%');
  });

  it('shows failed and active muted counts when non-zero', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({
        episodes_done: 5,
        episodes_total: 24,
        episodes_failed: 3,
        episodes_active: 1,
      }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('5/24')).toBeInTheDocument());

    expect(screen.getByText('3 failed · 1 active')).toBeInTheDocument();
  });

  it('hides the failed/active line when all zero', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({
        episodes_done: 12,
        episodes_total: 24,
        episodes_failed: 0,
        episodes_active: 0,
      }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('12/24')).toBeInTheDocument());

    expect(screen.queryByText(/failed/)).not.toBeInTheDocument();
    expect(screen.queryByText(/active/)).not.toBeInTheDocument();
  });

  it('renders "no jobs yet" when episodes_total is 0', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_total: 0 }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('no jobs yet')).toBeInTheDocument());

    // No bar for a zero-total series.
    expect(document.querySelector('.step-bar-fill')).toBeNull();
    expect(screen.queryByText(/failed/)).not.toBeInTheDocument();
  });
});

describe('Series page notify mute toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders a muted 🔕 button for a series with notify=false', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: false }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    // Muted series shows the muted icon button.
    const muteBtn = screen.getByRole('button', { name: /unmute/i });
    expect(muteBtn).toBeInTheDocument();
    expect(muteBtn.textContent).toContain('🔕');
  });

  it('renders an active 🔔 button for a series with notify=true (or absent)', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: true }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    const muteBtn = screen.getByRole('button', { name: /mute/i });
    expect(muteBtn).toBeInTheDocument();
    expect(muteBtn.textContent).toContain('🔔');
  });

  it('PATCHes notify:false when muting an unmuted series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: true }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ notify: false }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /mute/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { notify: false }),
    );
  });

  it('PATCHes notify:true when unmuting a muted series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: false }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ notify: true }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /unmute/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { notify: true }),
    );
  });

  it('applies a subtle visual hint (opacity) to muted series names', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: false }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    // The muted series name has reduced opacity.
    const nameCell = screen.getByText('Show A');
    expect(nameCell.style.opacity).toBe('0.7');
  });
});

describe('Series page pause toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders a ▶️ resume button for a paused series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: true }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    const pauseBtn = screen.getByRole('button', { name: /resume/i });
    expect(pauseBtn).toBeInTheDocument();
    expect(pauseBtn.textContent).toContain('▶️');
  });

  it('renders a ⏸️ pause button for an unpaused series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: false }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    const pauseBtn = screen.getByRole('button', { name: /^pause/i });
    expect(pauseBtn).toBeInTheDocument();
    expect(pauseBtn.textContent).toContain('⏸️');
  });

  it('PATCHes paused:true when pausing', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: false }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ paused: true }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /^pause/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { paused: true }),
    );
  });

  it('PATCHes paused:false when resuming', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: true }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ paused: false }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /resume/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { paused: false }),
    );
  });
});
