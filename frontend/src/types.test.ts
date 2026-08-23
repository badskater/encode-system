import { describe, expect, it } from 'vitest';
import type { StepTemplate, FlowExport } from './types';

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
});
