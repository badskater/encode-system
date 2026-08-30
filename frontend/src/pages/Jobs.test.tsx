import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import JobsPage from './Jobs';
import { api } from '../api/client';
import type { Job } from '../types';

// usePolling resolves fn() immediately on mount; mocking the api methods is
// enough to drive the page render without waiting for real timers.
function jobFixture(overrides: Partial<Job> = {}): Job {
  return {
    id: 1,
    series: 'Show',
    episode: 'Ep 01',
    episode_dir: '/data/Show/Ep 01',
    script_type: 'vpy',
    flow_id: 1,
    status: 'done',
    exit_code: 0,
    created_at: '2026-01-02T03:04:05Z',
    started_at: '2026-01-02T03:04:05Z',
    finished_at: '2026-01-02T04:00:00Z',
    ...overrides,
  };
}

describe('Jobs page', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('disables the Log button for a running job', async () => {
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 11, status: 'running' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('11')).toBeInTheDocument());

    const logBtn = screen.getByRole('button', { name: /log/i }) as HTMLButtonElement;
    expect(logBtn.disabled).toBe(true);
  });

  it('enables the Log button for a done job', async () => {
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 22, status: 'done' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('22')).toBeInTheDocument());

    const logBtn = screen.getByRole('button', { name: /log/i }) as HTMLButtonElement;
    expect(logBtn.disabled).toBe(false);
  });

  it('opens the log dialog when the Log button is clicked', async () => {
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 33, status: 'done' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    vi.spyOn(api, 'getJobLog').mockResolvedValue('the full log body');

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('33')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /log/i }));
    await waitFor(() => expect(api.getJobLog).toHaveBeenCalledWith(33));
    await waitFor(() =>
      expect(screen.getByText(/Job #33/)).toBeInTheDocument(),
    );
  });

  it('renders step timings in the detail card when the job is selected', async () => {
    const job = jobFixture({
      id: 44,
      status: 'done',
      step_timings: [
        { step: 'encode', started_at: '2026-01-02T03:04:05Z', duration_sec: 7705 },
        { step: 'mux', started_at: '2026-01-02T05:12:30Z', duration_sec: 45 },
      ],
    });
    vi.spyOn(api, 'jobs').mockResolvedValue([job]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('44')).toBeInTheDocument());

    // Click the job id link to open the detail card.
    fireEvent.click(screen.getByText('44'));
    await waitFor(() => expect(screen.getByText('Step timings')).toBeInTheDocument());
    expect(screen.getByText('encode')).toBeInTheDocument();
    expect(screen.getByText('2h 08m')).toBeInTheDocument();
  });
});
