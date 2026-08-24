import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import ChangePasswordDialog from './ChangePasswordDialog';
import { api } from '../api/client';

describe('ChangePasswordDialog', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('rejects mismatched new passwords client-side', async () => {
    const spy = vi.spyOn(api, 'changePassword');
    render(<ChangePasswordDialog onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/current password/i), { target: { value: 'old-secret-1' } });
    fireEvent.change(screen.getByLabelText(/new password \(min/i), { target: { value: 'new-secret-123' } });
    fireEvent.change(screen.getByLabelText(/confirm new password/i), { target: { value: 'different' } });
    fireEvent.click(screen.getByRole('button', { name: 'Change password' }));

    await waitFor(() => expect(screen.getByText('New passwords do not match.')).toBeInTheDocument());
    expect(spy).not.toHaveBeenCalled();
  });

  it('rejects short passwords client-side', async () => {
    const spy = vi.spyOn(api, 'changePassword');
    render(<ChangePasswordDialog onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/current password/i), { target: { value: 'old-secret-1' } });
    fireEvent.change(screen.getByLabelText(/new password \(min/i), { target: { value: 'short' } });
    fireEvent.change(screen.getByLabelText(/confirm new password/i), { target: { value: 'short' } });
    fireEvent.click(screen.getByRole('button', { name: 'Change password' }));

    await waitFor(() =>
      expect(screen.getByText('New password must be at least 10 characters.')).toBeInTheDocument(),
    );
    expect(spy).not.toHaveBeenCalled();
  });

  it('submits and shows the success note (remove .env hint)', async () => {
    const spy = vi.spyOn(api, 'changePassword').mockResolvedValue({ status: 'password updated' });
    render(<ChangePasswordDialog onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/current password/i), { target: { value: 'old-secret-1' } });
    fireEvent.change(screen.getByLabelText(/new password \(min/i), { target: { value: 'new-secret-123' } });
    fireEvent.change(screen.getByLabelText(/confirm new password/i), { target: { value: 'new-secret-123' } });
    fireEvent.click(screen.getByRole('button', { name: 'Change password' }));

    await waitFor(() => expect(screen.getByText('Password changed')).toBeInTheDocument());
    expect(spy).toHaveBeenCalledWith('old-secret-1', 'new-secret-123');
    // Success screen advises removing the env password.
    expect(screen.getByText(/ENCODE_ADMIN_PASSWORD/)).toBeInTheDocument();
  });

  it('surfaces server errors (wrong current password)', async () => {
    vi.spyOn(api, 'changePassword').mockRejectedValue(new Error('401: current password is incorrect'));
    render(<ChangePasswordDialog onClose={() => {}} />);

    fireEvent.change(screen.getByLabelText(/current password/i), { target: { value: 'wrong-old' } });
    fireEvent.change(screen.getByLabelText(/new password \(min/i), { target: { value: 'new-secret-123' } });
    fireEvent.change(screen.getByLabelText(/confirm new password/i), { target: { value: 'new-secret-123' } });
    fireEvent.click(screen.getByRole('button', { name: 'Change password' }));

    await waitFor(() => expect(screen.getByText('current password is incorrect')).toBeInTheDocument());
  });

  it('closes on backdrop click', () => {
    const onClose = vi.fn();
    const { container } = render(<ChangePasswordDialog onClose={onClose} />);
    const backdrop = container.querySelector('.modal-backdrop')!;
    fireEvent.click(backdrop);
    expect(onClose).toHaveBeenCalled();
  });
});
