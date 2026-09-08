import { useMemo, useState } from 'react';
import type { Flow, Step, StepTemplate } from '../types';

// RetryPolicy is the { max_retries, retry_backoff_minutes } pair the builder
// hands to onSave alongside name + steps. max_retries=0 means retry is OFF.
export interface RetryPolicy {
  max_retries: number;
  retry_backoff_minutes: number;
}

interface Props {
  initial: Flow | null;
  templates: StepTemplate[];
  onSave: (name: string, steps: Step[], policy: RetryPolicy) => Promise<void>;
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
  const [maxRetries, setMaxRetries] = useState(initial?.max_retries ?? 0);
  const [retryBackoff, setRetryBackoff] = useState(initial?.retry_backoff_minutes ?? 15);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canSave = useMemo(() => name.trim() !== '' && steps.length > 0, [name, steps]);

  // Retry policy validation: max_retries 0-10, backoff 1-1440 minutes.
  // Backoff is only meaningful when retries > 0, but we validate it
  // unconditionally so a stale value can't sneak through if the user
  // toggles retries back up later.
  const retryValid = useMemo(() => {
    // NaN (e.g. from a cleared number input on some browsers) must fail
    // validation — relational comparisons with NaN are all false and would
    // silently let it through to JSON.stringify → null on the wire.
    // Integer check: Go's json decoder rejects 2.5 into an int field, so a
    // fractional value that passes here would 400 on save — block it client-side.
    if (!Number.isInteger(maxRetries) || maxRetries < 0 || maxRetries > 10) return false;
    if (maxRetries > 0 && (!Number.isInteger(retryBackoff) || retryBackoff < 1 || retryBackoff > 1440))
      return false;
    return true;
  }, [maxRetries, retryBackoff]);

  const retryError = useMemo(() => {
    if (!Number.isInteger(maxRetries) || maxRetries < 0 || maxRetries > 10)
      return 'Max retries must be a whole number 0–10';
    if (maxRetries > 0 && (!Number.isInteger(retryBackoff) || retryBackoff < 1 || retryBackoff > 1440))
      return 'Retry backoff must be a whole number 1–1440 minutes';
    return null;
  }, [maxRetries, retryBackoff]);

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
    // Client-side validation of the retry policy before hitting the API.
    if (!retryValid) {
      setError(retryError);
      return;
    }
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
      await onSave(name.trim(), cleaned, {
        max_retries: maxRetries,
        retry_backoff_minutes: maxRetries > 0 ? retryBackoff : 0,
      });
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

      <div className="toolbar">
        <label>
          Max retries
          <input
            type="number"
            min={0}
            max={10}
            value={maxRetries}
            onChange={(e) => setMaxRetries(Number(e.target.value))}
            style={{ width: 80, marginLeft: 6 }}
          />
        </label>
        <label>
          Retry backoff (min)
          <input
            type="number"
            min={1}
            max={1440}
            value={retryBackoff}
            disabled={maxRetries === 0}
            onChange={(e) => setRetryBackoff(Number(e.target.value))}
            style={{ width: 80, marginLeft: 6 }}
          />
        </label>
      </div>

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
