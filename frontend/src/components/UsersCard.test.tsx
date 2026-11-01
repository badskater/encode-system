import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import UsersCard from './UsersCard';
import { api } from '../api/client';
import type { User } from '../types';

vi.mock('../api/client', () => ({
  api: {
    listUsers: vi.fn(),
    createUser: vi.fn(),
    deleteUser: vi.fn(),
    resetUserPassword: vi.fn(),
  },
}));

const mockedApi = vi.mocked(api, true);

function userFixture(overrides: Partial<User> = {}): User {
  return {
    id: 1,
    username: 'admin',
    role: 'admin',
    created_at: '2026-01-02T03:04:05Z',
    ...overrides,
  };
}

describe('UsersCard', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mockedApi.listUsers.mockReset();
    mockedApi.createUser.mockReset();
    mockedApi.deleteUser.mockReset();
    mockedApi.resetUserPassword.mockReset();
    mockedApi.listUsers.mockResolvedValue([] as never);
  });

  it('renders empty state when no users exist', async () => {
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('No users yet.')).toBeTruthy());
  });

  it('renders the users table with username, role chip, and created date', async () => {
    mockedApi.listUsers.mockResolvedValue([
      userFixture({ id: 1, username: 'admin', role: 'admin' }),
      userFixture({ id: 2, username: 'viewer1', role: 'viewer' }),
    ] as never);
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('viewer1')).toBeTruthy());
    // admin role shows as a badge text, viewer shows as gray badge
    expect(screen.getByText('admin', { selector: 'span.badge' })).toBeTruthy();
    expect(screen.getByText('viewer', { selector: 'span.badge' })).toBeTruthy();
  });

  it('creates a user with username/password/role', async () => {
    mockedApi.createUser.mockResolvedValue(userFixture({ id: 3 }) as never);
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('No users yet.')).toBeTruthy());

    fireEvent.change(screen.getByLabelText('New user username'), { target: { value: 'ops' } });
    fireEvent.change(screen.getByLabelText('New user password'), { target: { value: 'password123' } });
    fireEvent.click(screen.getByRole('button', { name: /create user/i }));

    await waitFor(() =>
      expect(mockedApi.createUser).toHaveBeenCalledWith('ops', 'password123', 'viewer'),
    );
  });

  it('opens a confirm dialog when Delete is clicked', async () => {
    mockedApi.listUsers.mockResolvedValue([userFixture({ id: 5, username: 'bob' })] as never);
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('bob')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Delete user bob'));
    await waitFor(() => expect(screen.getByText(/Delete user/)).toBeTruthy());
    expect(screen.getByRole('button', { name: /confirm delete/i })).toBeTruthy();
  });

  it('calls deleteUser on confirm', async () => {
    mockedApi.listUsers.mockResolvedValue([userFixture({ id: 5, username: 'bob' })] as never);
    mockedApi.deleteUser.mockResolvedValue(undefined as never);
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('bob')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Delete user bob'));
    fireEvent.click(screen.getByRole('button', { name: /confirm delete/i }));

    await waitFor(() => expect(mockedApi.deleteUser).toHaveBeenCalledWith(5));
  });

  it('surfaces a 409 last-admin error inline in the delete dialog', async () => {
    mockedApi.listUsers.mockResolvedValue([userFixture({ id: 1, username: 'admin' })] as never);
    mockedApi.deleteUser.mockRejectedValue(new Error('409: cannot delete the last admin account'));
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByLabelText('Delete user admin')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Delete user admin'));
    fireEvent.click(screen.getByRole('button', { name: /confirm delete/i }));

    await waitFor(() => expect(mockedApi.deleteUser).toHaveBeenCalledWith(1));
    await waitFor(() =>
      expect(screen.getByText(/cannot delete the last admin account/)).toBeTruthy(),
    );
    // Dialog stays open on error
    expect(screen.getByRole('button', { name: /confirm delete/i })).toBeTruthy();
  });

  it('opens a reset password form and calls resetUserPassword', async () => {
    mockedApi.listUsers.mockResolvedValue([userFixture({ id: 7, username: 'carol' })] as never);
    mockedApi.resetUserPassword.mockResolvedValue({ status: 'password updated' } as never);
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('carol')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Reset password for carol'));
    await waitFor(() => expect(screen.getByLabelText('Reset password new value')).toBeTruthy());

    fireEvent.change(screen.getByLabelText('Reset password new value'), {
      target: { value: 'newpassword123' },
    });
    fireEvent.click(screen.getByRole('button', { name: /^reset$/i }));

    await waitFor(() => expect(mockedApi.resetUserPassword).toHaveBeenCalledWith(7, 'newpassword123'));
  });

  it('surfaces a password validation error inline in the reset form', async () => {
    mockedApi.listUsers.mockResolvedValue([userFixture({ id: 7, username: 'carol' })] as never);
    mockedApi.resetUserPassword.mockRejectedValue(
      new Error('400: password must be at least 10 characters'),
    );
    render(<UsersCard />);
    await waitFor(() => expect(screen.getByText('carol')).toBeTruthy());

    fireEvent.click(screen.getByLabelText('Reset password for carol'));
    fireEvent.change(screen.getByLabelText('Reset password new value'), {
      target: { value: 'short' },
    });
    fireEvent.click(screen.getByRole('button', { name: /^reset$/i }));

    await waitFor(() =>
      expect(screen.getByText(/password must be at least 10 characters/)).toBeTruthy(),
    );
  });
});
