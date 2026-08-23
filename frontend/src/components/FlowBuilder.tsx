import { useMemo, useState } from 'react';
import type { Flow, Step, StepType } from '../types';
import { STEP_CATALOG, stepMeta } from '../types';

interface Props {
  initial: Flow | null;
  onSave: (name: string, steps: Step[]) => Promise<void>;
  onCancel: () => void;
}

// FlowBuilder is the visual editor: a palette of step templates on the right,
// an ordered step list on the left with reorder/remove controls and per-step
// parameter editing. Saving sends name + ordered steps to the controller,
// which validates the flow renders before persisting.
export default function FlowBuilder({ initial, onSave, onCancel }: Props) {
  const [name, setName] = useState(initial?.name ?? '');
  const [steps, setSteps] = useState<Step[]>(initial?.steps ?? []);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canSave = useMemo(() => name.trim() !== '' && steps.length > 0, [name, steps]);

  function addStep(type: StepType) {
    const meta = stepMeta(type);
    const params: Record<string, string> = {};
    for (const p of meta.params) params[p.key] = '';
    setSteps([...steps, { type, params }]);
  }

  function removeStep(i: number) {
    setSteps(steps.filter((_, idx) => idx !== i));
  }

  function moveStep(i: number, dir: -1 | 1) {
    const j = i + dir;
    if (j < 0 || j >= steps.length) return;
    const next = [...steps];
    [next[i], next[j]] = [next[j], next[i]];
    setSteps(next);
  }

  function setParam(i: number, key: string, value: string) {
    const next = steps.map((s, idx) =>
      idx === i ? { ...s, params: { ...(s.params ?? {}), [key]: value } } : s,
    );
    setSteps(next);
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      // Strip empty params so the backend applies step defaults.
      const cleaned = steps.map((s) => ({
        type: s.type,
        params: Object.fromEntries(
          Object.entries(s.params ?? {}).filter(([, v]) => v.trim() !== ''),
        ),
      }));
      await onSave(name.trim(), cleaned);
    } catch (e) {
      setError(String(e instanceof Error ? e.message : e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div>
      <div className="toolbar">
        <input
          placeholder="flow name (e.g. 1080p-opus)"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <button className="btn primary" disabled={!canSave || saving} onClick={save}>
          {saving ? 'Saving…' : initial ? 'Save changes' : 'Create flow'}
        </button>
        <button className="btn" onClick={onCancel}>
          Cancel
        </button>
      </div>
      {error && <div className="error-box">{error}</div>}

      <div className="flow-canvas">
        <div className="flow-steps">
          <h3>Pipeline steps (execution order)</h3>
          {steps.length === 0 && (
            <p className="muted">No steps yet — add from the palette on the right.</p>
          )}
          {steps.map((s, i) => {
            const meta = stepMeta(s.type);
            return (
              <div className="step-card" key={i}>
                <div className="step-num">{i + 1}</div>
                <div className="step-body">
                  <strong>{meta.label}</strong>
                  <div className="muted" style={{ fontSize: 12 }}>
                    {meta.description}
                  </div>
                  {meta.params.length > 0 && (
                    <div className="step-params">
                      {meta.params.map((p) => (
                        <label key={p.key}>
                          {p.label}
                          <input
                            value={s.params?.[p.key] ?? ''}
                            placeholder={p.placeholder ?? 'default'}
                            onChange={(e) => setParam(i, p.key, e.target.value)}
                          />
                        </label>
                      ))}
                    </div>
                  )}
                </div>
                <div className="step-actions">
                  <button title="Move up" onClick={() => moveStep(i, -1)} disabled={i === 0}>
                    ↑
                  </button>
                  <button
                    title="Move down"
                    onClick={() => moveStep(i, 1)}
                    disabled={i === steps.length - 1}
                  >
                    ↓
                  </button>
                  <button title="Remove" onClick={() => removeStep(i)}>
                    ×
                  </button>
                </div>
              </div>
            );
          })}
        </div>

        <div className="flow-palette">
          <h3>Add step</h3>
          {STEP_CATALOG.map((meta) => (
            <div key={meta.type} className="palette-step" onClick={() => addStep(meta.type)}>
              <strong>{meta.label}</strong>
              <div className="desc">{meta.description}</div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
