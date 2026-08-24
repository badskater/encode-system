import { useMemo, useState } from 'react';
import type { Flow, Step, StepTemplate } from '../types';

interface Props {
  initial: Flow | null;
  templates: StepTemplate[];
  onSave: (name: string, steps: Step[]) => Promise<void>;
  onCancel: () => void;
}

// stepMeta resolves template metadata; unknown keys degrade to their raw key.
function metaFor(templates: StepTemplate[], key: string) {
  const t = templates.find((x) => x.key === key);
  return (
    t ?? {
      id: 0,
      key,
      label: key,
      description: 'unknown template',
      params: [],
      powershell: '',
      builtin: false,
      created_at: '',
      updated_at: '',
    }
  );
}

// FlowBuilder is the visual editor: a palette of step templates on the right,
// an ordered step list on the left with reorder/remove controls and per-step
// parameter editing. Saving sends name + ordered steps to the controller,
// which validates the flow renders before persisting.
export default function FlowBuilder({ initial, templates, onSave, onCancel }: Props) {
  const [name, setName] = useState(initial?.name ?? '');
  const [steps, setSteps] = useState<Step[]>(initial?.steps ?? []);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canSave = useMemo(() => name.trim() !== '' && steps.length > 0, [name, steps]);

  function addStep(type: string) {
    const meta = metaFor(templates, type);
    const params: Record<string, string> = {};
    // Prefill from the template's declared defaults so the current values are
    // visible and editable in the builder (blank = script-side default).
    for (const p of meta.params) params[p.key] = p.default ?? '';
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
      // Strip empty params so the backend applies step defaults; keep
      // explicit values (including booleans) so the flow stays self-documenting.
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
            const meta = metaFor(templates, s.type);
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
                      {meta.params.map((p) =>
                        p.type === 'bool' ? (
                          <label key={p.key} className="param-bool">
                            <input
                              type="checkbox"
                              checked={(s.params?.[p.key] ?? p.default ?? 'false') === 'true'}
                              onChange={(e) => setParam(i, p.key, e.target.checked ? 'true' : 'false')}
                            />
                            {p.label}
                          </label>
                        ) : (
                          <label key={p.key}>
                            {p.label}
                            <input
                              type={p.type === 'number' ? 'number' : 'text'}
                              value={s.params?.[p.key] ?? ''}
                              placeholder={p.default ?? p.placeholder ?? 'default'}
                              onChange={(e) => setParam(i, p.key, e.target.value)}
                            />
                          </label>
                        ),
                      )}
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
          {templates.map((meta) => (
            <div
              key={meta.key}
              className="palette-step"
              onClick={() => addStep(meta.key)}
            >
              <strong>{meta.label}</strong>
              {!meta.builtin && <span className="badge green">custom</span>}
              <div className="desc">{meta.description}</div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
