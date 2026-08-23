import { describe, expect, it, vi, beforeEach } from 'vitest';
import { api, setToken, getToken } from './client';

describe('api client', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('stores and retrieves the admin token', () => {
    setToken('abc123');
    expect(getToken()).toBe('abc123');
  });

  it('sends the bearer header on requests', async () => {
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
    expect(init.headers.Authorization).toBe('Bearer secret-admin');
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
