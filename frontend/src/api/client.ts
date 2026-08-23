// Minimal fetch wrapper for the controller API. The admin bearer token is
// supplied at runtime (prompt on first load, persisted to localStorage).

import type { Flow, Job, JobStatus, Node, Settings } from '../types';

const TOKEN_KEY = 'enc…oken';

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? '';
}

export function setToken(t: string): void {
  localStorage.setItem(TOKEN_KEY, t);
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${getToken()}`,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const err = await res.json();
      if (err.error) msg = err.error;
    } catch {
      /* non-JSON error body */
    }
    throw new Error(`${res.status}: ${msg}`);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

export const api = {
  health: () => request<{ status: string }>('GET', '/api/health'),
  settings: () => request<Settings>('GET', '/api/settings'),

  nodes: () => request<Node[]>('GET', '/api/nodes'),
  createNode: (name: string) =>
    request<{ node: Node; token: string }>('POST', '/api/nodes', { name }),
  setNodeEnabled: (id: number, enabled: boolean) =>
    request<Node>('PATCH', `/api/nodes/${id}`, { enabled }),
  rebootNode: (id: number) => request<Node>('POST', `/api/nodes/${id}/reboot`),

  jobs: (status?: JobStatus) =>
    request<Job[]>('GET', `/api/jobs${status ? `?status=${status}` : ''}`),
  job: (id: number) => request<Job>('GET', `/api/jobs/${id}`),
  retryJob: (id: number) => request<Job>('POST', `/api/jobs/${id}/retry`),
  cancelJob: (id: number) => request<void>('POST', `/api/jobs/${id}/cancel`),
  createJob: (body: { series: string; episode_dir: string; script_type: string; flow_id?: number }) =>
    request<Job>('POST', '/api/jobs', body),

  flows: () => request<Flow[]>('GET', '/api/flows'),
  createFlow: (flow: Omit<Flow, 'id' | 'created_at' | 'updated_at'>) =>
    request<Flow>('POST', '/api/flows', flow),
  updateFlow: (id: number, flow: Partial<Pick<Flow, 'name' | 'steps'>>) =>
    request<Flow>('PUT', `/api/flows/${id}`, flow),
  deleteFlow: (id: number) => request<void>('DELETE', `/api/flows/${id}`),
};
