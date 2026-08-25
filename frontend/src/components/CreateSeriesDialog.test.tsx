import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import CreateSeriesDialog from './CreateSeriesDialog';
import { api } from '../api/client';
import type { CreateSeriesResponse } from '../types';

const madeResponse: CreateSeriesResponse = {
  series: {
    id: 7,
    name: 'New Show',
    flow_id: 0,
    tag: '2160p',
    enabled: true,
    created_at: '',
    updated_at: '',
  },
  scripts_folder: '/data/scripts/New Show',
  release_folder: '/data/release/[OldFartsSubs] New Show - Raws [2160p]',
  episode_folders: ['Ep 01', 'Ep 02'],
};

describe('CreateSeriesDialog', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('rejects an empty name client-side', async () => {
    const spy = vi.spyOn(api, 'createSeries');
    render(<CreateSeriesDialog flows={[]} onClose={() => {}} />);

    fireEvent.click(screen.getByRole('button', { name: 'Create series' }));
    await waitFor(() => expect(screen.getByText('Series name is required.')).toBeInTheDocument());
    expect(spy).not.toHaveBeenCalled();
  });

  it('submits name, episodes, tag and flow, then shows the summary', async () => {
    const spy = vi.spyOn(api, 'createSeries').mockResolvedValue(madeResponse);
    render(<CreateSeriesDialog flows={[]} onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/series name/i), { target: { value: 'New Show' } });
    fireEvent.change(screen.getByLabelText(/number of episodes/i), { target: { value: '2' } });
    fireEvent.change(screen.getByLabelText(/quality tag/i), { target: { value: '2160p' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create series' }));

    await waitFor(() => expect(screen.getByText('Series created')).toBeInTheDocument());
    expect(spy).toHaveBeenCalledWith({
      name: 'New Show',
      episodes: 2,
      tag: '2160p',
      flow_id: undefined,
    });
    // Summary shows the scaffolding locations.
    expect(screen.getByText(/Ep 01/)).toBeInTheDocument();
    expect(screen.getByText('New Show')).toBeInTheDocument();
  });

  it('omits blank tag and zero flow (server inherits defaults)', async () => {
    const spy = vi.spyOn(api, 'createSeries').mockResolvedValue(madeResponse);
    render(<CreateSeriesDialog flows={[]} onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/series name/i), { target: { value: 'Bare' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create series' }));

    await waitFor(() => expect(spy).toHaveBeenCalledWith({ name: 'Bare', episodes: 12 }));
  });

  it('surfaces server errors and stays mounted', async () => {
    vi.spyOn(api, 'createSeries').mockRejectedValue(new Error('name contains characters not allowed'));
    render(<CreateSeriesDialog flows={[]} onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/series name/i), { target: { value: 'bad/name' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create series' }));

    await waitFor(() =>
      expect(screen.getByText('name contains characters not allowed')).toBeInTheDocument(),
    );
    expect(screen.getByRole('heading', { name: 'Create series' })).toBeInTheDocument();
  });

  it('closes on backdrop click', () => {
    const onClose = vi.fn();
    const { container } = render(<CreateSeriesDialog flows={[]} onClose={onClose} />);
    fireEvent.click(container.querySelector('.modal-backdrop')!);
    expect(onClose).toHaveBeenCalled();
  });
});
