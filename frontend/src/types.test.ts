import { describe, expect, it } from 'vitest';
import type { StepTemplate, FlowExport, Job, Flow } from './types';

describe('step template types', () => {
  it('StepTemplate carries the PowerShell + params contract', () => {
    const t: StepTemplate = {
      id: 1,
      key: 'audio',
      label: 'Audio',
      description: 'eac3to to opusenc',
      params: [
        { key: 'track', label: 'Track index', placeholder: '2' },
        { key: 'bitrate', label: 'Opus bitrate (kbps)', placeholder: '320' },
      ],
      powershell: 'function Invoke-AudioExtract { param($Job, $Params) }',
      builtin: true,
      created_at: '',
      updated_at: '',
    };
    expect(t.key).toBe('audio');
    expect(t.params.map((p) => p.key)).toEqual(['track', 'bitrate']);
  });

  it('FlowExport embeds templates for portability', () => {
    const exp: FlowExport = {
      flow: { id: 0, name: 'x', steps: [], is_default: false, created_at: '', updated_at: '' },
      templates: [],
    };
    expect(exp.flow.is_default).toBe(false);
    expect(Array.isArray(exp.templates)).toBe(true);
  });

  it('Job carries priority and retry fields from Phase D2', () => {
    const job: Job = {
      id: 1,
      series: 'Show',
      episode: 'Ep 01',
      episode_dir: '/data/Show/Ep 01',
      script_type: 'vpy',
      flow_id: 1,
      status: 'pending',
      exit_code: 0,
      created_at: '',
      started_at: null,
      finished_at: null,
      priority: 1,
      retry_count: 2,
      next_retry_at: '2026-08-30T12:00:00Z',
    };
    expect(job.priority).toBe(1);
    expect(job.retry_count).toBe(2);
    expect(job.next_retry_at).toBe('2026-08-30T12:00:00Z');
  });

  it('Flow carries retry policy fields from Phase D2', () => {
    const flow: Flow = {
      id: 1,
      name: '1080p-opus',
      steps: [],
      is_default: false,
      created_at: '',
      updated_at: '',
      max_retries: 3,
      retry_backoff_minutes: 15,
    };
    expect(flow.max_retries).toBe(3);
    expect(flow.retry_backoff_minutes).toBe(15);
  });
});
