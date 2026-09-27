import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import BackupsCard from './BackupsCard';
import { api } from '../api/client';

vi.mock('../api/client', () => ({
  api: {
    backupStatus: vi.fn(),
    backupNow: vi.fn(),
    backupSettings: vi.fn(),
    deleteBackup: vi.fn(),
    downloadBackup: vi.fn(),
  },
}));

const mockedApi = vi.mocked(api, true);

const baseStatus = {
  status: { enabled: false, every_seconds: 21600, max_backups: 24 },
  snapshots: [],
};

describe('BackupsCard', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mockedApi.backupStatus.mockReset();
    mockedApi.backupNow.mockReset();
    mockedApi.backupSettings.mockReset();
    mockedApi.deleteBackup.mockReset();
    mockedApi.downloadBackup.mockReset();
    mockedApi.backupStatus.mockResolvedValue(baseStatus as never);
  });

  it('renders empty state and takes a manual snapshot', async () => {
    render(<BackupsCard />);
    await waitFor(() => expect(screen.getByText('No snapshots yet.')).toBeTruthy());

    mockedApi.backupNow.mockResolvedValue({
      name: 'encode-20260927-220000-manual.db',
      size_bytes: 40960,
      created_at: '2026-09-27T22:00:00Z',
      scheduled: false,
    } as never);
    mockedApi.backupStatus.mockResolvedValue({
      ...baseStatus,
      snapshots: [
        { name: 'encode-20260927-220000-manual.db', size_bytes: 40960, created_at: '2026-09-27T22:00:00Z', scheduled: false },
      ],
    } as never);

    fireEvent.click(screen.getByLabelText('Back up now'));
    await waitFor(() => expect(mockedApi.backupNow).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText('encode-20260927-220000-manual.db')).toBeTruthy());
    expect(screen.getByText('40 KiB')).toBeTruthy();
  });

  it('saves the schedule with hours→seconds conversion', async () => {
    mockedApi.backupSettings.mockResolvedValue({ enabled: true, every_seconds: 43200, max_backups: 24 } as never);
    render(<BackupsCard />);
    await waitFor(() => expect(screen.getByText('No snapshots yet.')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Scheduled backups enabled'));
    fireEvent.change(screen.getByLabelText('Backup interval hours'), { target: { value: '12' } });
    fireEvent.click(screen.getByRole('button', { name: /save schedule/i }));

    await waitFor(() => expect(mockedApi.backupSettings).toHaveBeenCalledWith(true, 43200));
  });

  it('deletes a snapshot', async () => {
    mockedApi.backupStatus.mockResolvedValue({
      ...baseStatus,
      snapshots: [
        { name: 'old.db', size_bytes: 1024, created_at: '2026-09-27T22:00:00Z', scheduled: true },
      ],
    } as never);
    mockedApi.deleteBackup.mockResolvedValue(undefined as never);

    render(<BackupsCard />);
    await waitFor(() => expect(screen.getByText('old.db')).toBeTruthy());
    expect(screen.getByText('(sched)')).toBeTruthy();

    fireEvent.click(screen.getByLabelText('Delete old.db'));
    await waitFor(() => expect(mockedApi.deleteBackup).toHaveBeenCalledWith('old.db'));
  });

  it('downloads a snapshot as a blob', async () => {
    mockedApi.backupStatus.mockResolvedValue({
      ...baseStatus,
      snapshots: [
        { name: 'dl.db', size_bytes: 1024, created_at: '2026-09-27T22:00:00Z', scheduled: false },
      ],
    } as never);
    mockedApi.downloadBackup.mockResolvedValue(new Blob(['x']) as never);
    // jsdom lacks URL.createObjectURL — stub it and the click anchor.
    const origCreate = URL.createObjectURL;
    URL.createObjectURL = vi.fn(() => 'blob:fake');
    URL.revokeObjectURL = vi.fn();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});

    render(<BackupsCard />);
    await waitFor(() => expect(screen.getByText('dl.db')).toBeTruthy());
    fireEvent.click(screen.getByLabelText('Download dl.db'));

    await waitFor(() => expect(mockedApi.downloadBackup).toHaveBeenCalledWith('dl.db'));
    await waitFor(() => expect(clickSpy).toHaveBeenCalled());
    clickSpy.mockRestore();
    URL.createObjectURL = origCreate;
  });
});
