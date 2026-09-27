import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import AuditPage from './Audit';
import { api } from '../api/client';

// Mock the API client — the page only calls listAudit.
vi.mock('../api/client', () => ({
  api: { listAudit: vi.fn() },
}));

const mockedApi = vi.mocked(api, true);

const rows = [
  { id: 3, at: '2026-09-27 20:00:00', actor: 'admin', action: 'series.update', object: 'series:5', detail: '{"paused":true}' },
  { id: 2, at: '2026-09-27 19:00:00', actor: 'admin', action: 'settings.update', object: 'settings', detail: '' },
  { id: 1, at: '2026-09-27 18:00:00', actor: 'admin', action: 'node.delete', object: 'node:2', detail: '{"name":"enc-02"}' },
];

describe('AuditPage', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mockedApi.listAudit.mockReset();
    mockedApi.listAudit.mockResolvedValue(rows as never);
  });

  it('renders audit rows newest-first with actor/action/object', async () => {
    render(<AuditPage />);
    expect(screen.getByText('Loading…')).toBeTruthy();
    await waitFor(() => expect(screen.queryByText('Loading…')).toBeNull());

    expect(screen.getByText('series.update')).toBeTruthy();
    expect(screen.getByText('settings.update')).toBeTruthy();
    expect(screen.getByText('node.delete')).toBeTruthy();
    expect(screen.getByText('node:2')).toBeTruthy();
    expect(screen.getAllByText('admin').length).toBe(3);
  });

  it('filters rows by action substring', async () => {
    render(<AuditPage />);
    await waitFor(() => expect(screen.getByText('series.update')).toBeTruthy());

    fireEvent.change(screen.getByLabelText('Filter audit events'), { target: { value: 'node' } });

    expect(screen.getByText('node.delete')).toBeTruthy();
    expect(screen.queryByText('series.update')).toBeNull();
    expect(screen.queryByText('settings.update')).toBeNull();
  });

  it('shows an empty state when no events exist', async () => {
    mockedApi.listAudit.mockResolvedValue([] as never);
    render(<AuditPage />);
    await waitFor(() => expect(screen.getByText('No audit events yet.')).toBeTruthy());
  });

  it('refreshes on button click', async () => {
    render(<AuditPage />);
    await waitFor(() => expect(screen.getByText('series.update')).toBeTruthy());
    expect(mockedApi.listAudit).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByLabelText('Refresh audit log'));
    await waitFor(() => expect(mockedApi.listAudit).toHaveBeenCalledTimes(2));
  });
});
