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

  it('renders a priority selector for pending jobs only', async () => {
    const pending = jobFixture({ id: 100, status: 'pending', priority: 0 });
    const running = jobFixture({ id: 101, status: 'running', priority: 0 });
    vi.spyOn(api, 'jobs').mockResolvedValue([pending, running]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('100')).toBeInTheDocument());

    // Pending job row has a priority <select> titled "Set job priority".
    const priSelect = screen.getByTitle('Set job priority');
    expect(priSelect.tagName).toBe('SELECT');
    // Running job row must NOT render its own priority select — the only
    // priority select on the page belongs to the pending job.
    expect(screen.getAllByTitle('Set job priority')).toHaveLength(1);
  });

  it('PATCHes priority when the selector changes on a pending job', async () => {
    const pending = jobFixture({ id: 200, status: 'pending', priority: 0 });
    vi.spyOn(api, 'jobs').mockResolvedValue([pending]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    const patchSpy = vi.spyOn(api, 'patchJob').mockResolvedValue(pending);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('200')).toBeInTheDocument());

    const priSelect = screen.getByTitle('Set job priority') as HTMLSelectElement;
    fireEvent.change(priSelect, { target: { value: '1' } });

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(200, { priority: 1 }),
    );
  });

  it('shows a High priority badge on non-pending jobs with priority 1', async () => {
    const done = jobFixture({ id: 300, status: 'done', priority: 1 });
    vi.spyOn(api, 'jobs').mockResolvedValue([done]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('300')).toBeInTheDocument());

    // Done job has no selector, but shows a static High badge.
    expect(screen.getByText('High')).toBeInTheDocument();
    expect(screen.queryByTitle('Set job priority')).not.toBeInTheDocument();
  });

  it('renders a retry count indicator when retry_count > 0', async () => {
    const job = jobFixture({ id: 400, status: 'running', retry_count: 2 });
    vi.spyOn(api, 'jobs').mockResolvedValue([job]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('400')).toBeInTheDocument());

    expect(screen.getByText(/retry 2/i)).toBeInTheDocument();
  });

  it('renders a backoff wait indicator when next_retry_at is in the future', async () => {
    // next_retry_at 10 minutes from now — formatted as HH:MM via fmtTime.
    const future = new Date(Date.now() + 10 * 60 * 1000);
    const job = jobFixture({
      id: 401,
      status: 'failed',
      retry_count: 1,
      next_retry_at: future.toISOString(),
    });
    vi.spyOn(api, 'jobs').mockResolvedValue([job]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('401')).toBeInTheDocument());

    // The wait text shows "waits until" with the HH:MM time from fmtTime.
    expect(screen.getByText(/waits until/i)).toBeInTheDocument();
  });

  it('does not render retry indicators when retry_count is 0', async () => {
    const job = jobFixture({ id: 402, status: 'running', retry_count: 0 });
    vi.spyOn(api, 'jobs').mockResolvedValue([job]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('402')).toBeInTheDocument());

    // No "retry N" indicator and no wait text for a fresh job.
    expect(screen.queryByText(/retry \d/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/waits until/i)).not.toBeInTheDocument();
  });

  it('selects rows and runs a bulk retry with affected/skipped feedback', async () => {
    vi.spyOn(api, 'jobs').mockResolvedValue([
      jobFixture({ id: 31, status: 'failed' }),
      jobFixture({ id: 32, status: 'running' }),
    ]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    const bulkSpy = vi.spyOn(api, 'bulkJobs').mockResolvedValue({
      action: 'retry', affected: 1, skipped: [32],
    });

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('31')).toBeInTheDocument());

    // No bulk buttons until something is checked.
    expect(screen.queryByRole('button', { name: /retry selected/i })).not.toBeInTheDocument();

    // Check both rows via the header select-all.
    fireEvent.click(screen.getByLabelText('Select all jobs'));
    expect(screen.getByText('2 selected')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /retry selected/i }));
    await waitFor(() => expect(bulkSpy).toHaveBeenCalledWith('retry', [31, 32]));
    await waitFor(() =>
      expect(screen.getByText('retry: 1 affected, 1 skipped')).toBeInTheDocument(),
    );
    // Selection clears after the op.
    expect(screen.queryByText('2 selected')).not.toBeInTheDocument();
  });

  it('bulk cancel uses the checked ids and clears on filter change', async () => {
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 41, status: 'pending' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    const bulkSpy = vi.spyOn(api, 'bulkJobs').mockResolvedValue({
      action: 'cancel', affected: 1, skipped: [],
    });

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('41')).toBeInTheDocument());

    fireEvent.click(screen.getByLabelText('Select job 41'));
    // Changing the filter drops the selection instead of acting on hidden rows.
    // getAllByRole: pending rows carry their own flow/priority selects —
    // the filter select is the first combobox on the page.
    const filterSelect = screen.getAllByRole('combobox')[0];
    fireEvent.change(filterSelect, { target: { value: 'pending' } });
    expect(screen.queryByText('1 selected')).not.toBeInTheDocument();

    // Re-check and cancel.
    fireEvent.click(screen.getByLabelText('Select job 41'));
    fireEvent.click(screen.getByRole('button', { name: /cancel selected/i }));
    await waitFor(() => expect(bulkSpy).toHaveBeenCalledWith('cancel', [41]));
    await waitFor(() => expect(screen.getByText('cancel: 1 affected')).toBeInTheDocument());
  });
});

// Deep-link handling (Phase F2): when the Jobs page mounts with ?job=<id> in
// the URL (e.g. clicked from a Discord alert), it auto-opens that job's log
// dialog. The param is consumed once via a ref so it never re-triggers on
// subsequent polls.
describe('Jobs page deep-link (?job=…)', () => {
  const originalSearch = window.location.search;

  beforeEach(() => {
    vi.restoreAllMocks();
  });

  afterEach(() => {
    // Restore the original search so other tests aren't affected.
    window.history.replaceState({}, '', originalSearch);
  });

  it('auto-opens the log dialog for the job id in ?job= and shows the fetched log', async () => {
    window.history.replaceState({}, '', '/jobs?job=2');
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 2, status: 'done' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    vi.spyOn(api, 'getJobLog').mockResolvedValue('the deep-linked log body');

    render(<JobsPage />);
    await waitFor(() => expect(api.getJobLog).toHaveBeenCalledWith(2));
    await waitFor(() =>
      expect(screen.getByText(/Job #2/)).toBeInTheDocument(),
    );
    expect(screen.getByText('the deep-linked log body')).toBeInTheDocument();
  });

  it('does not auto-open when no ?job= param is present', async () => {
    window.history.replaceState({}, '', '/jobs');
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 50, status: 'done' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    const logSpy = vi.spyOn(api, 'getJobLog').mockResolvedValue('log text');

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('50')).toBeInTheDocument());

    // No log fetch fired automatically — the dialog wasn't auto-opened.
    expect(logSpy).not.toHaveBeenCalled();
  });

  it('does not auto-open when the job id is not in the polled list', async () => {
    window.history.replaceState({}, '', '/jobs?job=999');
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 60, status: 'done' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    const logSpy = vi.spyOn(api, 'getJobLog').mockResolvedValue('log text');

    render(<JobsPage />);
    await waitFor(() => expect(screen.getByText('60')).toBeInTheDocument());

    // The job isn't in the list, so the dialog never auto-opens.
    expect(logSpy).not.toHaveBeenCalled();
  });

  it('consumes the param once — does not re-open on a subsequent poll', async () => {
    window.history.replaceState({}, '', '/jobs?job=70');
    vi.spyOn(api, 'jobs').mockResolvedValue([jobFixture({ id: 70, status: 'done' })]);
    vi.spyOn(api, 'nodes').mockResolvedValue([]);
    vi.spyOn(api, 'flows').mockResolvedValue([]);
    const logSpy = vi.spyOn(api, 'getJobLog').mockResolvedValue('first open');

    render(<JobsPage />);
    await waitFor(() => expect(logSpy).toHaveBeenCalledTimes(1));

    // Fire a manual re-poll (usePolling re-runs fn). After the first poll
    // the ref guard should prevent re-opening.
    // Advance fake timers to trigger the next poll tick.
    vi.useFakeTimers();
    vi.advanceTimersByTime(5000);
    vi.useRealTimers();
    // Give the async state update a chance to flush.
    await waitFor(() => expect(logSpy).toHaveBeenCalledTimes(1));
  });
});
