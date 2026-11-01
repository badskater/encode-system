import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import FlowsPage from './Flows';
import { api } from '../api/client';
import { RoleContext } from '../hooks/useRole';
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

describe('Flows page viewer gating', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('hides the New flow and Import JSON buttons for viewers', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([flowFixture()]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);

    render(<RoleContext.Provider value="viewer"><FlowsPage /></RoleContext.Provider>);
    await waitFor(() => expect(screen.getByText('1080p-opus')).toBeInTheDocument());

    expect(screen.queryByRole('button', { name: /new flow/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /import json/i })).not.toBeInTheDocument();
  });

  it('hides Edit/Duplicate/Delete buttons for viewers but keeps Export', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([flowFixture()]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);

    render(<RoleContext.Provider value="viewer"><FlowsPage /></RoleContext.Provider>);
    await waitFor(() => expect(screen.getByText('1080p-opus')).toBeInTheDocument());

    expect(screen.queryByRole('button', { name: /edit/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /duplicate/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
    // Export is read-only — stays visible
    expect(screen.getByRole('button', { name: /export/i })).toBeInTheDocument();
  });
});

describe('Flows page duplicate action', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('calls duplicateFlow with the prompted name', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([flowFixture({ id: 5, name: 'my-flow' })]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);
    const dupSpy = vi.spyOn(api, 'duplicateFlow').mockResolvedValue(
      flowFixture({ id: 6, name: 'my-flow (copy)' }),
    );
    // prompt returns the default name
    vi.stubGlobal('prompt', vi.fn(() => 'my-flow (copy)'));

    render(<FlowsPage />);
    await waitFor(() => expect(screen.getByText('my-flow')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /duplicate/i }));
    await waitFor(() => expect(dupSpy).toHaveBeenCalledWith(5, 'my-flow (copy)'));
    vi.unstubAllGlobals();
  });

  it('does not call duplicateFlow when prompt is cancelled', async () => {
    vi.spyOn(api, 'flows').mockResolvedValue([flowFixture({ id: 5, name: 'my-flow' })]);
    vi.spyOn(api, 'stepTemplates').mockResolvedValue([]);
    const dupSpy = vi.spyOn(api, 'duplicateFlow').mockResolvedValue(flowFixture({ id: 6 }));
    vi.stubGlobal('prompt', vi.fn(() => null));

    render(<FlowsPage />);
    await waitFor(() => expect(screen.getByText('my-flow')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /duplicate/i }));
    expect(dupSpy).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });
});
