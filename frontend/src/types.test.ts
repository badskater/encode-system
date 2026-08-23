import { describe, expect, it } from 'vitest';
import { STEP_CATALOG, stepMeta } from './types';

describe('step catalog', () => {
  it('covers all seven pipeline steps', () => {
    expect(STEP_CATALOG.map((s) => s.type)).toEqual([
      'source_rename',
      'dgindex',
      'audio',
      'encode',
      'mux',
      'release_copy',
      'keyframes',
    ]);
  });

  it('resolves metadata for every step type', () => {
    for (const s of STEP_CATALOG) {
      const meta = stepMeta(s.type);
      expect(meta.label).toBeTruthy();
      expect(meta.description).toBeTruthy();
    }
  });

  it('audio step exposes track and bitrate params', () => {
    const audio = stepMeta('audio');
    expect(audio.params.map((p) => p.key)).toEqual(['track', 'bitrate']);
  });
});
