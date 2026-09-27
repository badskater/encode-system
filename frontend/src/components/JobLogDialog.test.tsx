import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import JobLogDialog from './JobLogDialog';
import { api } from '../api/client';
import type { StepTiming } from '../types';

describe('JobLogDialog', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    // ETA is fetched best-effort on each progress event; default to
    // "no estimate" so live-tail tests don't need to care about it.
    vi.spyOn(api, 'jobETA').mockResolvedValue({
      avg_sec: 0, samples: 0, elapsed_sec: 0, eta_sec: -1,
    });
  });

  it('renders a loading state then the log text', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('line one\nline two');
    render(<JobLogDialog jobId={42} jobLabel="Show Ep 01" onClose={() => {}} />);

    // Loading indicator appears first.
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByText(/loading/i)).not.toBeInTheDocument(),
    );
    // The log lives in a <pre> with horizontal scroll (no wrap).
    const pre = document.querySelector('pre')!;
    expect(pre).not.toBeNull();
    expect(pre.textContent).toContain('line one');
    expect(pre.textContent).toContain('line two');
  });

  it('shows the heading label including the job id and label', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('ok');
    render(<JobLogDialog jobId={42} jobLabel="Show Ep 01" onClose={() => {}} />);
    await waitFor(() => expect(screen.getByText(/Job #42/)).toBeInTheDocument());
    expect(screen.getByText(/Show Ep 01/)).toBeInTheDocument();
  });

  it('surfaces the server error message on rejection', async () => {
    vi.spyOn(api, 'getJobLog').mockRejectedValue(
      new Error('404: no log recorded for this job'),
    );
    render(<JobLogDialog jobId={7} jobLabel="x" onClose={() => {}} />);
    await waitFor(() =>
      expect(screen.getByText(/no log recorded for this job/)).toBeInTheDocument(),
    );
    // Dialog stays mounted.
    expect(screen.getByRole('heading', { name: /Job #7/ })).toBeInTheDocument();
  });

  it('renders step timings above the log when provided', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('log');
    const timings: StepTiming[] = [
      { step: 'encode', started_at: '2026-01-02T03:04:05Z', duration_sec: 7705 },
      { step: 'mux', started_at: '2026-01-02T05:12:30Z', duration_sec: 45 },
    ];
    render(
      <JobLogDialog
        jobId={1}
        jobLabel="x"
        onClose={() => {}}
        stepTimings={timings}
      />,
    );
    await waitFor(() => expect(screen.getByText('Step timings')).toBeInTheDocument());
    expect(screen.getByText('encode')).toBeInTheDocument();
    expect(screen.getByText('2h 08m')).toBeInTheDocument();
    expect(screen.getByText('mux')).toBeInTheDocument();
  });

  it('downloads the log as job-<id>.log via a blob URL', async () => {
    const logText = 'download me';
    vi.spyOn(api, 'getJobLog').mockResolvedValue(logText);
    // jsdom omits URL.createObjectURL; provide a stub so the dialog can build
    // the blob URL, then assert on the anchor it synthesizes.
    const createUrl = vi.fn().mockReturnValue('blob:test-url');
    const revokeUrl = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL: createUrl, revokeObjectURL: revokeUrl });
    const clickSpy = vi.fn();
    const anchorProto = HTMLAnchorElement.prototype;
    const realClick = anchorProto.click;
    anchorProto.click = vi.fn(function (this: HTMLAnchorElement) {
      expect(this.href).toBe('blob:test-url');
      expect(this.download).toBe('job-9.log');
      clickSpy();
    });
    render(<JobLogDialog jobId={9} jobLabel="x" onClose={() => {}} />);
    await waitFor(() => {
      const pre = document.querySelector('pre');
      expect(pre?.textContent).toContain(logText);
    });
    fireEvent.click(screen.getByRole('button', { name: /download/i }));
    expect(createUrl).toHaveBeenCalledOnce();
    expect(clickSpy).toHaveBeenCalled();
    anchorProto.click = realClick;
  });

  it('closes on backdrop click when not loading', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('log');
    const onClose = vi.fn();
    const { container } = render(
      <JobLogDialog jobId={1} jobLabel="x" onClose={onClose} />,
    );
    await waitFor(() => expect(screen.getByText('log')).toBeInTheDocument());
    fireEvent.click(container.querySelector('.modal-backdrop')!);
    expect(onClose).toHaveBeenCalled();
  });

  it('does not close on backdrop click while loading', async () => {
    let resolveLog: (v: string) => void = () => {};
    vi.spyOn(api, 'getJobLog').mockReturnValue(
      new Promise<string>((r) => {
        resolveLog = r;
      }),
    );
    const onClose = vi.fn();
    const { container } = render(
      <JobLogDialog jobId={1} jobLabel="x" onClose={onClose} />,
    );
    // Still loading: backdrop click must not close.
    fireEvent.click(container.querySelector('.modal-backdrop')!);
    expect(onClose).not.toHaveBeenCalled();
    // Let the pending fetch resolve so React settles before unmount.
    resolveLog('log');
    await waitFor(() =>
      expect(container.querySelector('pre')?.textContent).toContain('log'),
    );
  });

  it('closes on the close button', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('log');
    const onClose = vi.fn();
    render(<JobLogDialog jobId={1} jobLabel="x" onClose={onClose} />);
    await waitFor(() => expect(screen.getByText('log')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /^close$/i }));
    expect(onClose).toHaveBeenCalled();
  });

  it('closes on Escape when not loading', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('log');
    const onClose = vi.fn();
    render(<JobLogDialog jobId={1} jobLabel="x" onClose={onClose} />);
    await waitFor(() => expect(screen.getByText('log')).toBeInTheDocument());
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });
  it('streams live progress for a running job and refetches the log on final', async () => {
    // getJobLog: 404 while running (no captured log yet), real log after final.
    let calls = 0;
    vi.spyOn(api, 'getJobLog').mockImplementation(async () => {
      calls += 1;
      if (calls === 1) throw new Error('404: no log recorded for this job');
      return 'captured full log';
    });
    // Capture the stream callback so the test can push events.
    let pushEvent: ((ev: unknown) => void) | null = null;
    vi.spyOn(api, 'streamJobLog').mockImplementation(
      async (_id, onEvent) => {
        pushEvent = onEvent as (ev: unknown) => void;
        // Never resolves until the test pushes the final event; the dialog
        // aborts on unmount, so a pending promise is safe here.
        await new Promise<void>(() => {});
      },
    );

    render(
      <JobLogDialog jobId={9} jobLabel="Show Ep 02" onClose={() => {}} jobStatus="running" />,
    );

    // The 404 on a running job must NOT surface as an error box.
    await waitFor(() => expect(screen.queryByText(/loading/i)).not.toBeInTheDocument());
    expect(screen.queryByText(/no log recorded/)).not.toBeInTheDocument();

    // A live progress event renders the tail + step/percent header.
    pushEvent!({ type: 'progress', step: 'encode', progress: 42, log_tail: 'frame 1234' });
    await waitFor(() => expect(screen.getByText('frame 1234')).toBeInTheDocument());
    expect(screen.getByText(/Live tail — encode \(42%\)/)).toBeInTheDocument();

    // The final event triggers a refetch of the captured log.
    pushEvent!({ type: 'final', status: 'done', exit_code: 0, full_log: true });
    await waitFor(() => expect(screen.getByText('captured full log')).toBeInTheDocument());
  });

  it('shows the ETA line when the stream reports progress and history exists', async () => {
    vi.spyOn(api, 'getJobLog').mockRejectedValue(new Error('404: no log'));
    vi.spyOn(api, 'jobETA').mockResolvedValue({
      avg_sec: 1200, samples: 5, elapsed_sec: 300, eta_sec: 900, progress: 25,
    });
    let pushEvent: ((ev: unknown) => void) | null = null;
    vi.spyOn(api, 'streamJobLog').mockImplementation(async (_id, onEvent) => {
      pushEvent = onEvent as (ev: unknown) => void;
      await new Promise<void>(() => {});
    });

    render(<JobLogDialog jobId={11} jobLabel="x" onClose={() => {}} jobStatus="running" />);
    pushEvent!({ type: 'progress', step: 'encode', progress: 25, log_tail: 'frame 1' });

    // ~15m left, avg 20m over 5 runs
    await waitFor(() => expect(screen.getByText(/~15m left/)).toBeInTheDocument());
    expect(screen.getByText(/avg 20m over 5 runs/)).toBeInTheDocument();
  });

  it('hides the ETA line when there is insufficient history', async () => {
    vi.spyOn(api, 'getJobLog').mockRejectedValue(new Error('404: no log'));
    // samples < 2 → no estimate shown even though eta_sec >= 0
    vi.spyOn(api, 'jobETA').mockResolvedValue({
      avg_sec: 600, samples: 1, elapsed_sec: 0, eta_sec: 600,
    });
    let pushEvent: ((ev: unknown) => void) | null = null;
    vi.spyOn(api, 'streamJobLog').mockImplementation(async (_id, onEvent) => {
      pushEvent = onEvent as (ev: unknown) => void;
      await new Promise<void>(() => {});
    });
    render(<JobLogDialog jobId={12} jobLabel="x" onClose={() => {}} jobStatus="running" />);
    pushEvent!({ type: 'progress', step: 'encode', progress: 10, log_tail: 'frame 1' });
    await waitFor(() => expect(screen.getByText('frame 1')).toBeInTheDocument());
    expect(screen.queryByText(/left/)).not.toBeInTheDocument();
  });

  it('does not open a stream for a terminal job', async () => {
    vi.spyOn(api, 'getJobLog').mockResolvedValue('old log');
    const streamSpy = vi.spyOn(api, 'streamJobLog');
    render(
      <JobLogDialog jobId={3} jobLabel="x" onClose={() => {}} jobStatus="done" />,
    );
    await waitFor(() => expect(screen.getByText('old log')).toBeInTheDocument());
    expect(streamSpy).not.toHaveBeenCalled();
  });
});
