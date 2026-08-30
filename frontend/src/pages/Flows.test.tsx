import { render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import FlowsPage from './Flows';
import { api } from '../api/client';
import type { Flow } from '../types';

// usePolling resolves fn() immediately on mount; mocking the api methods is
// enough to drive the page render without waiting for real timers.
function flowFixture(overrides: Partial<Flow> = {}): Flow {
  return {
    id: 1,
    name: '1080p-opus',
    steps: [{ type: 'encode' }],
    is_default: false,
    created_at: '',
    updated_at: '',
    ...overrides,
  };
}

describe('Flows page', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the retry column header', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([flowFixture()]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);

    render(<FlowsPage />);
    await waitFor(() => expect(screen.getByText('1080p-opus')).toBeInTheDocument());

    expect(screen.getByText('Retry')).toBeInTheDocument();
  });

  it('shows "no retry" for a flow with no retry policy', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([flowFixture()]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);

    render(<FlowsPage />);
    await waitFor(() => expect(screen.getByText('no retry')).toBeInTheDocument());

    expect(screen.getByText('no retry')).toBeInTheDocument();
  });

  it('shows "retry Nx @Ym" for a flow with a retry policy', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([
      flowFixture({ id: 2, max_retries: 3, retry_backoff_minutes: 15 }),
    ]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);

    render(<FlowsPage />);
    await waitFor(() => expect(screen.getByText('retry 3× @15m')).toBeInTheDocument());

    expect(screen.getByText('retry 3× @15m')).toBeInTheDocument();
  });
});
