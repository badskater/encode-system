import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import FlowBuilder from './FlowBuilder';
import type { Flow } from '../types';

describe('FlowBuilder', () => {
  it('adds steps from the palette in order', () => {
    const onSave = vi.fn();
    render(<FlowBuilder initial={null} onSave={onSave} onCancel={() => {}} />);

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
    render(<FlowBuilder initial={null} onSave={onSave} onCancel={() => {}} />);

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
      created_at: '',
      updated_at: '',
    };
    render(<FlowBuilder initial={flow} onSave={vi.fn()} onCancel={() => {}} />);

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
      created_at: '',
      updated_at: '',
    };
    render(<FlowBuilder initial={flow} onSave={vi.fn()} onCancel={() => {}} />);

    fireEvent.click(screen.getAllByTitle('Remove')[0]);
    // Only one numbered step remains.
    expect(screen.queryByText('1')).toBeInTheDocument();
    expect(screen.queryByText('2')).not.toBeInTheDocument();
  });

  it('disables save without name or steps', () => {
    render(<FlowBuilder initial={null} onSave={vi.fn()} onCancel={() => {}} />);
    expect(screen.getByText('Create flow')).toBeDisabled();
  });
});
