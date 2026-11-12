import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import DeleteSeriesDialog from './DeleteSeriesDialog';
import { api } from '../api/client';
import type { Series } from '../types';

function seriesFixture(overrides: Partial<Series> = {}): Series {
  return {
    id: 7,
    name: 'Finished Show',
    flow_id: 0,
    tag: '',
    enabled: true,
    jobs: 24,
    episodes_done: 12,
    episodes_total: 24,
    created_at: '2026-01-02T03:04:05Z',
    updated_at: '2026-01-02T03:04:05Z',
    ...overrides,
  };
}

describe('DeleteSeriesDialog', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the series name and its progress counts', () => {
    render(
      <DeleteSeriesDialog series={seriesFixture()} onClose={() => {}} onDeleted={() => {}} />,
    );
    expect(screen.getByText('Finished Show')).toBeInTheDocument();
    // done/total episodes and the job-history count from GET /api/series.
    expect(screen.getByText('12/24 episodes encoded')).toBeInTheDocument();
    expect(screen.getByText('24')).toBeInTheDocument();
    // The registry-only semantics + scanner re-registration note are visible.
    expect(screen.getByText(/jobs, history and files on the shares are kept/i)).toBeInTheDocument();
    expect(screen.getByText(/re-registers it on the next scan/i)).toBeInTheDocument();
  });

  it('renders zero counts when the series has no jobs yet', () => {
    render(
      <DeleteSeriesDialog
        series={seriesFixture({ jobs: undefined, episodes_done: undefined, episodes_total: undefined })}
        onClose={() => {}}
        onDeleted={() => {}}
      />,
    );
    expect(screen.getByText('0/0 episodes encoded')).toBeInTheDocument();
  });

  it('confirm calls DELETE with the series id and reports success', async () => {
    const spy = vi.spyOn(api, 'deleteSeries').mockResolvedValue(undefined);
    const onDeleted = vi.fn();
    const onClose = vi.fn();
    render(
      <DeleteSeriesDialog series={seriesFixture()} onClose={onClose} onDeleted={onDeleted} />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Delete series' }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith(7));
    await waitFor(() => expect(onDeleted).toHaveBeenCalledTimes(1));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('cancel closes without calling the API', () => {
    const spy = vi.spyOn(api, 'deleteSeries');
    const onClose = vi.fn();
    const onDeleted = vi.fn();
    render(
      <DeleteSeriesDialog series={seriesFixture()} onClose={onClose} onDeleted={onDeleted} />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(spy).not.toHaveBeenCalled();
    expect(onDeleted).not.toHaveBeenCalled();
  });

  it('surfaces server errors and stays mounted', async () => {
    vi.spyOn(api, 'deleteSeries').mockRejectedValue(new Error('admin role required'));
    const onDeleted = vi.fn();
    render(
      <DeleteSeriesDialog series={seriesFixture()} onClose={() => {}} onDeleted={onDeleted} />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Delete series' }));
    await waitFor(() => expect(screen.getByText('admin role required')).toBeInTheDocument());
    expect(screen.getByRole('heading', { name: 'Delete series' })).toBeInTheDocument();
    expect(onDeleted).not.toHaveBeenCalled();
  });

  it('double-click only fires one DELETE (busy guard)', async () => {
    let resolveDelete: () => void = () => {};
    const spy = vi.spyOn(api, 'deleteSeries').mockImplementation(
      () => new Promise<void>((resolve) => { resolveDelete = resolve; }),
    );
    const onDeleted = vi.fn();
    render(
      <DeleteSeriesDialog series={seriesFixture()} onClose={() => {}} onDeleted={onDeleted} />,
    );
    const btn = screen.getByRole('button', { name: 'Delete series' });
    fireEvent.click(btn);
    // While the first request is in flight the button is disabled and
    // relabelled; a second click must not fire another DELETE.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Deleting…' })).toBeDisabled());
    fireEvent.click(btn);
    expect(spy).toHaveBeenCalledTimes(1);
    resolveDelete();
    await waitFor(() => expect(onDeleted).toHaveBeenCalledTimes(1));
  });
});