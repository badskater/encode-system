import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import APITokensCard from './APITokensCard';
import { api } from '../api/client';

vi.mock('../api/client', () => ({
  api: {
    listTokens: vi.fn(),
    createToken: vi.fn(),
    deleteToken: vi.fn(),
  },
}));

const mockedApi = vi.mocked(api, true);

describe('APITokensCard', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mockedApi.listTokens.mockReset();
    mockedApi.createToken.mockReset();
    mockedApi.deleteToken.mockReset();
    mockedApi.listTokens.mockResolvedValue([] as never);
  });

  it('shows the empty state and creates a token, revealing plaintext once', async () => {
    render(<APITokensCard />);
    await waitFor(() => expect(screen.getByText('No tokens yet.')).toBeTruthy());

    mockedApi.createToken.mockResolvedValue({ id: 1, name: 'sonarr', scope: 'read', token: 'PLAINTEXT-XYZ' } as never);
    mockedApi.listTokens.mockResolvedValue([
      { id: 1, name: 'sonarr', scope: 'read', last_used_at: null, created_at: '2026-09-27T20:00:00Z' },
    ] as never);

    const user = fireEvent;
    user.change(screen.getByLabelText('Token name'), { target: { value: 'sonarr' } });
    user.click(screen.getByRole('button', { name: /create token/i }));

    await waitFor(() => expect(screen.getByTestId('issued-token').textContent).toBe('PLAINTEXT-XYZ'));
    expect(mockedApi.createToken).toHaveBeenCalledWith('sonarr', 'read');
    // list refreshed after create
    await waitFor(() => expect(screen.getByText('sonarr')).toBeTruthy());
  });

  it('passes the selected scope through', async () => {
    render(<APITokensCard />);
    await waitFor(() => expect(screen.getByText('No tokens yet.')).toBeTruthy());

    mockedApi.createToken.mockResolvedValue({ id: 2, name: 'admin-bot', scope: 'admin', token: 'T' } as never);
    fireEvent.change(screen.getByLabelText('Token name'), { target: { value: 'admin-bot' } });
    fireEvent.change(screen.getByLabelText('Token scope'), { target: { value: 'admin' } });
    fireEvent.click(screen.getByRole('button', { name: /create token/i }));

    await waitFor(() => expect(mockedApi.createToken).toHaveBeenCalledWith('admin-bot', 'admin'));
  });

  it('revokes a token', async () => {
    mockedApi.listTokens.mockResolvedValue([
      { id: 7, name: 'old', scope: 'read', last_used_at: null, created_at: '2026-09-27T20:00:00Z' },
    ] as never);
    mockedApi.deleteToken.mockResolvedValue(undefined as never);

    render(<APITokensCard />);
    await waitFor(() => expect(screen.getByText('old')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Delete token old'));
    await waitFor(() => expect(mockedApi.deleteToken).toHaveBeenCalledWith(7));
  });

  it('surfaces create errors', async () => {
    render(<APITokensCard />);
    await waitFor(() => expect(screen.getByText('No tokens yet.')).toBeTruthy());

    mockedApi.createToken.mockRejectedValue(new Error('409: token name taken') as never);
    fireEvent.change(screen.getByLabelText('Token name'), { target: { value: 'dupe' } });
    fireEvent.click(screen.getByRole('button', { name: /create token/i }));

    await waitFor(() => expect(screen.getByText(/token name taken/)).toBeTruthy());
  });
});
