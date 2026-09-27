import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import NodesPage from './Nodes';
import { api } from '../api/client';
import type { Node } from '../types';

function nodeFixture(overrides: Partial<Node> = {}): Node {
  return {
    id: 1,
    name: 'enc-01',
    enabled: true,
    status: 'idle',
    group: '',
    agent_version: '1.0.0',
    lib_version: 3,
    tasks_since_boot: 2,
    reboot_pending: false,
    last_seen: '2026-01-02T03:04:05Z',
    ...overrides,
  };
}

describe('Nodes page group cell', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'pairingCodes').mockResolvedValue([]);
  });

  it('shows the group label when set', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([nodeFixture({ group: 'gpu' })]);

    render(<NodesPage />);
    await waitFor(() => expect(screen.getByText('gpu')).toBeInTheDocument());
  });

  it('renders the "any" hint for a wildcard node', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([nodeFixture({ group: '' })]);

    render(<NodesPage />);
    await waitFor(() => expect(screen.getByText('enc-01')).toBeInTheDocument());
    expect(screen.getByText('any')).toBeInTheDocument();
  });

  it('PATCHes group on inline edit', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([nodeFixture({ group: '' })]);
    const patchSpy = vi
      .spyOn(api, 'setNodeGroup')
      .mockResolvedValue(nodeFixture({ group: 'gpu' }));

    render(<NodesPage />);
    await waitFor(() => expect(screen.getByText('enc-01')).toBeInTheDocument());

    fireEvent.click(screen.getByText('any'));
    const input = screen.getByPlaceholderText('any');
    fireEvent.change(input, { target: { value: 'gpu' } });
    fireEvent.keyDown(input, { key: 'Enter' });

    await waitFor(() => expect(patchSpy).toHaveBeenCalledWith(1, 'gpu'));
  });
});

describe('Nodes page concurrency slots', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'pairingCodes').mockResolvedValue([]);
  });

  it('shows active/max slots and defaults max to 1', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([
      nodeFixture({ max_concurrent_jobs: 2, active_jobs: 1 }),
    ]);

    render(<NodesPage />);
    await waitFor(() => expect(screen.getByText('1/2')).toBeInTheDocument());
  });

  it('PATCHes max_concurrent_jobs when the select changes', async () => {
    vi.spyOn(api, 'nodes').mockResolvedValue([nodeFixture({ max_concurrent_jobs: 1 })]);
    const patchSpy = vi
      .spyOn(api, 'setNodeConcurrency')
      .mockResolvedValue(nodeFixture({ max_concurrent_jobs: 3 }));

    render(<NodesPage />);
    const select = await screen.findByLabelText(/max concurrent jobs/i);
    fireEvent.change(select, { target: { value: '3' } });

    await waitFor(() => expect(patchSpy).toHaveBeenCalledWith(1, 3));
  });
});
