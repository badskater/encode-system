import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import TokenGate from './TokenGate';
import { api } from '../api/client';

// The SSO button is env-gated: it renders ONLY when the controller's
// unauthenticated /api/health bootstrap reports oidc_enabled=true.
describe('TokenGate SSO button', () => {
  const originalLocation = window.location;
  let assignSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
    assignSpy = vi.fn();
    // jsdom does not implement navigation — replace location with a stub
    // exposing just what the component uses (assign).
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign: assignSpy, hash: '', pathname: '/', search: '' },
    });
  });

  afterEach(() => {
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: originalLocation,
    });
  });

  it('shows the SSO button when health reports oidc_enabled', async () => {
    vi.spyOn(api, 'health').mockResolvedValue({ status: 'ok', oidc_enabled: true });
    render(<TokenGate onDone={() => {}} />);
    expect(await screen.findByRole('button', { name: 'Sign in with SSO' })).toBeInTheDocument();
    // Local login form stays available alongside SSO.
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
  });

  it('hides the SSO button when oidc_enabled is false', async () => {
    const healthSpy = vi.spyOn(api, 'health').mockResolvedValue({ status: 'ok', oidc_enabled: false });
    render(<TokenGate onDone={() => {}} />);
    await waitFor(() => expect(healthSpy).toHaveBeenCalled());
    expect(screen.queryByRole('button', { name: /sign in with sso/i })).toBeNull();
  });

  it('hides the SSO button when the field is absent (older controller)', async () => {
    const healthSpy = vi.spyOn(api, 'health').mockResolvedValue({ status: 'ok' });
    render(<TokenGate onDone={() => {}} />);
    await waitFor(() => expect(healthSpy).toHaveBeenCalled());
    expect(screen.queryByRole('button', { name: /sign in with sso/i })).toBeNull();
  });

  it('hides the SSO button when the health probe fails', async () => {
    const healthSpy = vi.spyOn(api, 'health').mockRejectedValue(new Error('network down'));
    render(<TokenGate onDone={() => {}} />);
    await waitFor(() => expect(healthSpy).toHaveBeenCalled());
    expect(screen.queryByRole('button', { name: /sign in with sso/i })).toBeNull();
    // Local login is unaffected by the failed probe.
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('starts the backend OIDC flow on click', async () => {
    vi.spyOn(api, 'health').mockResolvedValue({ status: 'ok', oidc_enabled: true });
    render(<TokenGate onDone={() => {}} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in with SSO' }));
    expect(assignSpy).toHaveBeenCalledWith('/api/auth/oidc/login');
  });

  it('local login still works with SSO enabled', async () => {
    vi.spyOn(api, 'health').mockResolvedValue({ status: 'ok', oidc_enabled: true });
    const loginSpy = vi.spyOn(api, 'login').mockResolvedValue({
      token: 'local-session-token',
      username: 'admin',
      expires_at: '2026-11-13T00:00:00Z',
    });
    const onDone = vi.fn();
    render(<TokenGate onDone={onDone} />);
    await screen.findByRole('button', { name: 'Sign in with SSO' });

    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: 'admin' } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'local-password-1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));

    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(loginSpy).toHaveBeenCalledWith('admin', 'local-password-1');
    expect(localStorage.getItem('encode-session-token')).toBe('local-session-token');
  });
});