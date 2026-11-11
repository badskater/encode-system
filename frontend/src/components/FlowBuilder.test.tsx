import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import FlowBuilder from './FlowBuilder';
import type { Flow, StepTemplate } from '../types';

// Templates fixture standing in for the API-provided catalog.
function templatesFixture(): StepTemplate[] {
  const mk = (key: string, label: string, params: { key: string; label: string; placeholder?: string }[] = []) => ({
    id: 0,
    key,
    label,
    description: `${label} step`,
    params,
    powershell: '',
    builtin: true,
    created_at: '',
    updated_at: '',
  });
  return [
    mk('source_rename', 'Rename source', [{ key: 'source_name', label: 'Source name', placeholder: 'src' }]),
    mk('dgindex', 'DGIndexNV index'),
    mk('audio', 'Audio (eac3to → Opus)', [
      { key: 'track', label: 'Track index', placeholder: '2' },
      { key: 'bitrate', label: 'Opus bitrate (kbps)', placeholder: '320' },
    ]),
    mk('encode', 'x265 encode', [{ key: 'x265_args', label: 'x265 arguments' }]),
    mk('mux', 'MKV mux'),
    mk('release_copy', 'Release copy'),
    mk('keyframes', 'Keyframes'),
  ];
}

describe('FlowBuilder', () => {
  it('adds steps from the palette in order', () => {
    const onSave = vi.fn();
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);

    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.click(screen.getByText('Audio (eac3to → Opus)'));

    expect(screen.getByText('Pipeline steps (execution order)')).toBeInTheDocument();
    expect(screen.getByText('x265 encode')).toBeInTheDocument(); // palette still shows it
    // Step list shows numbered entries.
    expect(screen.getByText('1')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
  });

  it('strips empty params and calls onSave with ordered steps', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);

    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'my-flow' },
    });
    fireEvent.click(screen.getByText('Audio (eac3to → Opus)'));
    // Set only the bitrate param; track stays empty and must be stripped.
    fireEvent.change(screen.getByPlaceholderText('320'), { target: { value: '192' } });

    fireEvent.click(screen.getByText('Create flow'));

    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [name, steps] = onSave.mock.calls[0];
    expect(name).toBe('my-flow');
    expect(steps).toEqual([{ type: 'audio', params: { bitrate: '192' } }]);
  });

  it('reorders steps with move buttons', () => {
    const flow: Flow = {
      id: 1,
      name: 'existing',
      steps: [
        { type: 'dgindex' },
        { type: 'mux' },
      ],
      is_default: false,
      created_at: '',
      updated_at: '',
    };
    render(<FlowBuilder initial={flow} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);

    // Move step 2 (mux) up.
    const upButtons = screen.getAllByTitle('Move up');
    fireEvent.click(upButtons[1]);

    const cards = screen.getAllByText((_, el) => el?.tagName === 'STRONG').map((e) => e.textContent);
    expect(cards.slice(0, 2)).toEqual(['MKV mux', 'DGIndexNV index']);
  });

  it('removes steps', () => {
    const flow: Flow = {
      id: 1,
      name: 'x',
      steps: [{ type: 'dgindex' }, { type: 'mux' }],
      is_default: false,
      created_at: '',
      updated_at: '',
    };
    render(<FlowBuilder initial={flow} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);

    fireEvent.click(screen.getAllByTitle('Remove')[0]);
    // Only one numbered step remains.
    expect(screen.queryByText('1')).toBeInTheDocument();
    expect(screen.queryByText('2')).not.toBeInTheDocument();
  });

  it('disables save without name or steps', () => {
    render(
      <FlowBuilder initial={null} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />,
    );
    expect(screen.getByText('Create flow')).toBeDisabled();
  });

  it('prefills declared defaults when adding a step', () => {
    const withDefaults: StepTemplate[] = [
      {
        id: 0,
        key: 'encode',
        label: 'x265 encode',
        description: '',
        params: [
          { key: 'preset', label: 'Preset', default: 'slow' },
          { key: 'crf', label: 'CRF (quality)', default: '15', type: 'number' },
          { key: 'no_sao', label: 'Disable SAO', default: 'true', type: 'bool' },
        ],
        powershell: '',
        builtin: true,
        created_at: '',
        updated_at: '',
      },
    ];
    render(<FlowBuilder initial={null} templates={withDefaults} onSave={vi.fn()} onCancel={() => {}} />);

    fireEvent.click(screen.getByText('x265 encode'));

    // Text/number inputs prefilled with the declared defaults.
    expect((screen.getByDisplayValue('slow') as HTMLInputElement).value).toBe('slow');
    // Query by label, not displayValue: the builder's retry_backoff_minutes
    // field also defaults to 15, so getByDisplayValue('15') is now ambiguous.
    // Targeting the CRF param by its label is more precise, not weaker.
    expect((screen.getByLabelText('CRF (quality)') as HTMLInputElement).value).toBe('15');
    // Bool renders as a checked checkbox.
    const cb = screen.getByLabelText('Disable SAO') as HTMLInputElement;
    expect(cb.type).toBe('checkbox');
    expect(cb.checked).toBe(true);
  });

  it('renders max retries and backoff fields', () => {
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    expect(screen.getByLabelText(/max retries/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/backoff/i)).toBeInTheDocument();
  });

  it('disables backoff input when max retries is 0', () => {
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    const backoff = screen.getByLabelText(/backoff/i) as HTMLInputElement;
    expect(backoff.disabled).toBe(true);
  });

  it('enables backoff input when max retries > 0', () => {
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    const retries = screen.getByLabelText(/max retries/i) as HTMLInputElement;
    fireEvent.change(retries, { target: { value: '3' } });
    const backoff = screen.getByLabelText(/backoff/i) as HTMLInputElement;
    expect(backoff.disabled).toBe(false);
  });

  it('passes retry policy in onSave payload', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);

    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'retry-flow' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText(/max retries/i), { target: { value: '3' } });
    fireEvent.change(screen.getByLabelText(/backoff/i), { target: { value: '15' } });

    fireEvent.click(screen.getByText('Create flow'));

    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [, , policy] = onSave.mock.calls[0];
    expect(policy).toEqual({ max_retries: 3, retry_backoff_minutes: 15, job_timeout_minutes: 0 });
  });

  it('blocks save when max retries exceeds 10', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);

    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'bad' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText(/max retries/i), { target: { value: '11' } });

    fireEvent.click(screen.getByText('Create flow'));
    // onSave must NOT be called — validation blocks it.
    await waitFor(() => expect(screen.getByText(/retries must be a whole number/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });

  it('blocks save when backoff is below 1', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);

    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'bad2' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText(/max retries/i), { target: { value: '3' } });
    fireEvent.change(screen.getByLabelText(/backoff/i), { target: { value: '0' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(screen.getByText(/backoff must be a whole number/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });

  it('blocks save when retry values are fractional (Go int fields reject them)', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);

    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'frac' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    // type=number allows arbitrary values via fireEvent.change regardless of step
    fireEvent.change(screen.getByLabelText(/max retries/i), { target: { value: '2.5' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(screen.getByText(/retries must be a whole number/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });
});

describe('FlowBuilder timeouts', () => {
  it('renders the job timeout field defaulting to 0 (off)', () => {
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    const input = screen.getByLabelText(/job timeout/i) as HTMLInputElement;
    expect(input).toBeInTheDocument();
    expect(input.value).toBe('0');
  });

  it('prefills the job timeout from the initial flow', () => {
    const flow: Flow = {
      id: 1, name: 'existing', steps: [{ type: 'encode' }],
      is_default: false, created_at: '', updated_at: '',
      job_timeout_minutes: 90,
    };
    render(<FlowBuilder initial={flow} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    expect((screen.getByLabelText(/job timeout/i) as HTMLInputElement).value).toBe('90');
  });

  it('passes job_timeout_minutes in the onSave policy', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'timeout-flow' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText(/job timeout/i), { target: { value: '120' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [, , policy] = onSave.mock.calls[0];
    expect(policy).toEqual({ max_retries: 0, retry_backoff_minutes: 0, job_timeout_minutes: 120 });
  });

  it('blocks save when the job timeout exceeds 10080', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'bad-timeout' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText(/job timeout/i), { target: { value: '10081' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(screen.getByText(/job timeout must be a whole number/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });

  it('blocks save when the job timeout is fractional', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'frac-timeout' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText(/job timeout/i), { target: { value: '2.5' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(screen.getByText(/job timeout must be a whole number/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });

  it('renders a per-step timeout input for every step', () => {
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.click(screen.getByText('MKV mux'));
    // Exact label match: the flow-level field is "Job timeout (minutes)".
    expect(screen.getAllByLabelText('Timeout (minutes)')).toHaveLength(2);
  });

  it('prefills step timeouts from the initial flow', () => {
    const flow: Flow = {
      id: 1, name: 'existing',
      steps: [{ type: 'encode', timeout_minutes: 30 }, { type: 'mux' }],
      is_default: false, created_at: '', updated_at: '',
    };
    render(<FlowBuilder initial={flow} templates={templatesFixture()} onSave={vi.fn()} onCancel={() => {}} />);
    const inputs = screen.getAllByLabelText('Timeout (minutes)') as HTMLInputElement[];
    expect(inputs[0].value).toBe('30');
    expect(inputs[1].value).toBe('0');
  });

  it('includes timeout_minutes in the save payload when set', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'step-timeout-flow' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText('Timeout (minutes)'), { target: { value: '45' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [, steps] = onSave.mock.calls[0];
    expect(steps).toEqual([{ type: 'dgindex', params: {}, timeout_minutes: 45 }]);
  });

  it('omits timeout_minutes from the payload when 0 (inherit)', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'no-step-timeout' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [, steps] = onSave.mock.calls[0];
    expect(steps).toEqual([{ type: 'dgindex', params: {} }]);
  });

  it('blocks save when a step timeout exceeds 10080', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<FlowBuilder initial={null} templates={templatesFixture()} onSave={onSave} onCancel={() => {}} />);
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'bad-step-timeout' },
    });
    fireEvent.click(screen.getByText('DGIndexNV index'));
    fireEvent.change(screen.getByLabelText('Timeout (minutes)'), { target: { value: '10081' } });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(screen.getByText(/step 1 timeout must be a whole number/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });
});

// quality_gate (P5): the builtin surfaces through the same generic
// ParamDef-driven rendering as every other builtin — no bespoke UI. The
// fixture mirrors the REAL template params from
// backend/internal/flow/builtin_steps_quality.go (metric/threshold/sampling).
describe('FlowBuilder quality_gate params', () => {
  function qualityGateTemplate(): StepTemplate {
    return {
      id: 0,
      key: 'quality_gate',
      label: 'Quality gate (VMAF/SSIM)',
      description: 'Perceptual quality check against the source.',
      params: [
        { key: 'metric', label: 'Metric (vmaf or ssim)', default: 'vmaf' },
        {
          key: 'threshold',
          label: 'Minimum score (blank: 90 VMAF / 0.95 SSIM)',
          type: 'number',
          placeholder: '90',
        },
        { key: 'sampling', label: 'Compare every Nth frame (1 = all)', type: 'number', default: '12' },
      ],
      powershell: '',
      builtin: true,
      created_at: '',
      updated_at: '',
    };
  }

  it('shows quality_gate in the palette and renders its three params with defaults', () => {
    render(
      <FlowBuilder
        initial={null}
        templates={[qualityGateTemplate()]}
        onSave={vi.fn()}
        onCancel={() => {}}
      />,
    );
    fireEvent.click(screen.getByText('Quality gate (VMAF/SSIM)'));

    // metric + sampling prefilled from the declared defaults; threshold is
    // metric-dependent so it stays blank (script-side default) with the
    // VMAF placeholder visible.
    expect((screen.getByLabelText('Metric (vmaf or ssim)') as HTMLInputElement).value).toBe('vmaf');
    expect((screen.getByLabelText('Compare every Nth frame (1 = all)') as HTMLInputElement).value).toBe('12');
    const threshold = screen.getByLabelText('Minimum score (blank: 90 VMAF / 0.95 SSIM)') as HTMLInputElement;
    expect(threshold.value).toBe('');
    expect(threshold.type).toBe('number');
    expect(threshold.placeholder).toBe('90');
  });

  it('round-trips edited quality_gate params through onSave', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(
      <FlowBuilder
        initial={null}
        templates={[qualityGateTemplate()]}
        onSave={onSave}
        onCancel={() => {}}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'gated-flow' },
    });
    fireEvent.click(screen.getByText('Quality gate (VMAF/SSIM)'));
    fireEvent.change(screen.getByLabelText('Metric (vmaf or ssim)'), { target: { value: 'ssim' } });
    fireEvent.change(screen.getByLabelText('Minimum score (blank: 90 VMAF / 0.95 SSIM)'), {
      target: { value: '0.9' },
    });
    fireEvent.change(screen.getByLabelText('Compare every Nth frame (1 = all)'), {
      target: { value: '24' },
    });

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [name, steps] = onSave.mock.calls[0];
    expect(name).toBe('gated-flow');
    expect(steps).toEqual([
      { type: 'quality_gate', params: { metric: 'ssim', threshold: '0.9', sampling: '24' } },
    ]);
  });

  it('strips a blank threshold so the script applies the metric default', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(
      <FlowBuilder
        initial={null}
        templates={[qualityGateTemplate()]}
        onSave={onSave}
        onCancel={() => {}}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText('flow name (e.g. 1080p-opus)'), {
      target: { value: 'default-threshold' },
    });
    fireEvent.click(screen.getByText('Quality gate (VMAF/SSIM)'));

    fireEvent.click(screen.getByText('Create flow'));
    await waitFor(() => expect(onSave).toHaveBeenCalled());
    const [, steps] = onSave.mock.calls[0];
    // threshold blank -> stripped; metric/sampling keep their visible defaults.
    expect(steps).toEqual([
      { type: 'quality_gate', params: { metric: 'vmaf', sampling: '12' } },
    ]);
  });

  it('renders stored quality_gate params when editing an existing flow', () => {
    const flow: Flow = {
      id: 1,
      name: 'existing',
      steps: [
        { type: 'mux' },
        { type: 'quality_gate', params: { metric: 'ssim', threshold: '0.97', sampling: '6' } },
      ],
      is_default: false,
      created_at: '',
      updated_at: '',
    };
    render(
      <FlowBuilder
        initial={flow}
        templates={[qualityGateTemplate()]}
        onSave={vi.fn()}
        onCancel={() => {}}
      />,
    );
    expect((screen.getByLabelText('Metric (vmaf or ssim)') as HTMLInputElement).value).toBe('ssim');
    expect((screen.getByLabelText('Minimum score (blank: 90 VMAF / 0.95 SSIM)') as HTMLInputElement).value).toBe('0.97');
    expect((screen.getByLabelText('Compare every Nth frame (1 = all)') as HTMLInputElement).value).toBe('6');
  });
});
