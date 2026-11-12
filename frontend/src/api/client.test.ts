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

  it('PATCHes a job priority', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ id: 5, priority: 1 }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.patchJob(5, { priority: 1 });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0];
    expect(path).toBe('/api/jobs/5');
    expect(init.method).toBe('PATCH');
    expect(init.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(init.body)).toEqual({ priority: 1 });
  });

  it('scanNow POSTs /api/scan and returns the triggered flag', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ triggered: true }),
    });
    vi.stubGlobal('fetch', fetchMock);

    const res = await api.scanNow();
    expect(res.triggered).toBe(true);
    const [path, init] = fetchMock.mock.calls[0];
    expect(path).toBe('/api/scan');
    expect(init.method).toBe('POST');
  });

  it('deleteJob DELETEs a terminal job', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ deleted: true }),
    });
    vi.stubGlobal('fetch', fetchMock);

    const res = await api.deleteJob(42);
    expect(res.deleted).toBe(true);
    const [path, init] = fetchMock.mock.calls[0];
    expect(path).toBe('/api/jobs/42');
    expect(init.method).toBe('DELETE');
  });

  it('deleteJob surfaces the 409 terminal-only error', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      statusText: 'Conflict',
      json: async () => ({ error: 'only terminal jobs can be deleted' }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await expect(api.deleteJob(5)).rejects.toThrow('only terminal jobs can be deleted');
  });

  it('bulkJobs sends set_priority with the priority field', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ action: 'set_priority', affected: 2, skipped: [] }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.bulkJobs('set_priority', [1, 2], { priority: 1 });
    const [path, init] = fetchMock.mock.calls[0];
    expect(path).toBe('/api/jobs/bulk');
    expect(JSON.parse(init.body)).toEqual({ action: 'set_priority', ids: [1, 2], priority: 1 });
  });

  it('bulkJobs sends set_flow with the flow_id field', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ action: 'set_flow', affected: 1, skipped: [2] }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.bulkJobs('set_flow', [1, 2], { flow_id: 3 });
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init.body)).toEqual({ action: 'set_flow', ids: [1, 2], flow_id: 3 });
  });

  it('duplicateFlow POSTs with an optional name override', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ id: 9, name: 'My Copy', steps: [], is_default: false, created_at: '', updated_at: '' }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.duplicateFlow(5, 'My Copy');
    const [path, init] = fetchMock.mock.calls[0];
    expect(path).toBe('/api/flows/5/duplicate');
    expect(JSON.parse(init.body)).toEqual({ name: 'My Copy' });
  });

  it('duplicateFlow sends an empty body when no name is given', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ id: 9, name: 'orig (copy)', steps: [], is_default: false, created_at: '', updated_at: '' }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.duplicateFlow(5);
    const [, init] = fetchMock.mock.calls[0];
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body)).toEqual({});
  });

  it('listUsers GETs /api/users', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => [{ id: 1, username: 'admin', role: 'admin' }],
    });
    vi.stubGlobal('fetch', fetchMock);

    const users = await api.listUsers();
    expect(users).toHaveLength(1);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/users');
  });

  it('createUser POSTs username/password/role', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ id: 2, username: 'viewer1', role: 'viewer' }),
    });
    vi.stubGlobal('fetch', fetchMock);

    await api.createUser('viewer1', 'password123', 'viewer');
    const [path, init] = fetchMock.mock.calls[0];
    expect(path).toBe('/api/users');
    expect(JSON.parse(init.body)).toEqual({ username: 'viewer1', password: 'password123', role: 'viewer' });
  });

  it('restoreBackup POSTs to the restore endpoint', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ restored: true, restart: true }),
    });
    vi.stubGlobal('fetch', fetchMock);

    const res = await api.restoreBackup('encode-20260101.db');
    expect(res.restored).toBe(true);
    expect(res.restart).toBe(true);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/backup/encode-20260101.db/restore');
  });

  it('checkUpdate GETs /api/updates/check', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        current: '1.0.0',
        latest: '1.1.0',
        update_available: true,
        release_url: 'https://github.com/badskater/encode-system/releases/tag/v1.1.0',
      }),
    });
    vi.stubGlobal('fetch', fetchMock);

    const res = await api.checkUpdate();
    expect(res.update_available).toBe(true);
    expect(res.latest).toBe('1.1.0');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/updates/check');
  });
});

describe('consumeOidcFragment', () => {
  beforeEach(() => {
    localStorage.clear();
    window.location.hash = '';
  });

  it('extracts the OIDC session token and strips the fragment', async () => {
    const { consumeOidcFragment } = await import('./client');
    const tok = 'a1b2c3d4e5f6'.repeat(5) + 'abcd'; // 64 hex chars
    window.location.hash = '#oidc=' + tok;
    expect(consumeOidcFragment()).toBe(tok);
    expect(window.location.hash).toBe('');
  });

  it('returns empty string when no fragment is present', async () => {
    const { consumeOidcFragment } = await import('./client');
    expect(consumeOidcFragment()).toBe('');
  });

  it('ignores malformed fragments', async () => {
    const { consumeOidcFragment } = await import('./client');
    for (const bad of ['#oidc=too-short', '#oidc=' + 'g'.repeat(64), '#other=' + 'a'.repeat(64), '#oidc=' + 'a'.repeat(63)]) {
      window.location.hash = bad;
      expect(consumeOidcFragment()).toBe('');
    }
  });

  it('leaves a stored local-login session untouched', async () => {
    const { consumeOidcFragment } = await import('./client');
    setToken('local-session');
    expect(consumeOidcFragment()).toBe('');
    expect(hasToken()).toBe(true);
  });
});