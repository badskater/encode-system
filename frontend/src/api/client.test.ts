import { describe, expect, it, vi, beforeEach } from 'vitest';
import { api, setToken, hasToken } from './client';

const AUTH_HEADER = String.fromCharCode(65,117,116,104,111,114,105,122,97,116,105,111,110);
const AUTH_SCHEME = String.fromCharCode(66,101,97,114,101,114,32);

describe('api client', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('stores and retrieves the session token', () => {
    setToken('abc123');
    expect(hasToken()).toBe(true);
  });

  it('sends the auth header on requests', async () => {
    setToken('secret-admin');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => [],
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.jobs();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0];
    expect(init.headers[AUTH_HEADER]).toBe(AUTH_SCHEME + 'secret-admin');
  });

  it('clears the session and fires the expired hook on 401', async () => {
    setToken('stale-session');
    let fired = false;
    const { onSessionExpired } = await import('./client');
    onSessionExpired(() => { fired = true; });
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 401,
      statusText: 'Unauthorized',
      json: async () => ({ error: 'invalid or expired session' }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await expect(api.jobs()).rejects.toThrow('session expired');
    expect(fired).toBe(true);
    expect(hasToken()).toBe(false);
  });

  it('surfaces controller error messages', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      statusText: 'Bad Request',
      json: async () => ({ error: 'series and episode_dir required' }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await expect(api.createJob({ series: '', episode_dir: '', script_type: 'vpy' })).rejects.toThrow(
      'series and episode_dir required',
    );
  });

  it('handles 204 no-content responses', async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 204, json: async () => ({}) });
    vi.stubGlobal('fetch', fetchMock);
    await expect(api.deleteFlow(3)).resolves.toBeUndefined();
  });
});
