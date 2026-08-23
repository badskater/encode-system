import { useState } from 'react';
import { api } from '../api/client';
import type { StepTemplate } from '../types';
import { usePolling } from '../hooks/usePolling';

// StepsPage manages step templates: each pipeline section owns its PowerShell
// here. Built-ins are editable (their script is admin-owned) but not
// deletable/renamable; custom templates can be created, edited, and deleted.
export default function StepsPage() {
  const { data: templates, error } = usePolling<StepTemplate[]>(() => api.stepTemplates(), 30000);
  const [editing, setEditing] = useState<StepTemplate | null>(null);
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  async function save(t: StepTemplate, isNew: boolean) {
    try {
      if (isNew) {
        await api.createStepTemplate({
          key: t.key,
          label: t.label,
          description: t.description,
          params: t.params,
          powershell: t.powershell,
        });
      } else {
        await api.updateStepTemplate(t.id, {
          label: t.label,
          description: t.description,
          params: t.params,
          powershell: t.powershell,
        });
      }
      setEditing(null);
      setCreating(false);
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  async function remove(t: StepTemplate) {
    try {
      await api.deleteStepTemplate(t.id);
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  if (creating || editing) {
    return (
      <TemplateEditor
        initial={editing ?? undefined}
        onSave={save}
        onCancel={() => {
          setCreating(false);
          setEditing(null);
        }}
      />
    );
  }

  return (
    <>
      <h2>Step templates</h2>
      {error && <div className="error-box">{error}</div>}
      {actionError && <div className="error-box">{actionError}</div>}

      <p className="muted">
        Every flow section is backed by its own PowerShell function. When a flow
        runs, the controller links each referenced function into the final job
        script. Built-ins ship with the controller; create custom steps to extend
        pipelines (they appear in the flow builder automatically).
      </p>

      <div className="toolbar">
        <button className="btn primary" onClick={() => setCreating(true)}>
          New step template
        </button>
      </div>

      <table>
        <thead>
          <tr>
            <th>Key</th>
            <th>Label</th>
            <th>Type</th>
            <th>Params</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(templates ?? []).map((t) => (
            <tr key={t.id}>
              <td>
                <code>{t.key}</code>
              </td>
              <td>{t.label}</td>
              <td>
                <span className={`badge ${t.builtin ? 'blue' : 'green'}`}>
                  {t.builtin ? 'built-in' : 'custom'}
                </span>
              </td>
              <td className="muted">{t.params.map((p) => p.key).join(', ') || '—'}</td>
              <td>
                <button className="btn" onClick={() => setEditing(t)}>
                  Edit
                </button>{' '}
                {!t.builtin && (
                  <button className="btn danger" onClick={() => remove(t)}>
                    Delete
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

function TemplateEditor({
  initial,
  onSave,
  onCancel,
}: {
  initial?: StepTemplate;
  onSave: (t: StepTemplate, isNew: boolean) => Promise<void>;
  onCancel: () => void;
}) {
  const [t, setT] = useState<StepTemplate>(
    initial ?? {
      id: 0,
      key: '',
      label: '',
      description: '',
      params: [],
      powershell:
        'function Invoke-MyStep {\n' +
        '    param(\n' +
        '        [Parameter(Mandatory=$true)] [pscustomobject] $Job,\n' +
        '        [pscustomobject] $Params\n' +
        '    )\n' +
        "    Write-Output \"ENCODE_STEP mystep 0\"\n" +
        '    # ... your logic, using $Job.Series/$Job.EpisodeDir/Resolve-Tool etc.\n' +
        "    Write-Output \"ENCODE_STEP mystep 100\"\n" +
        '}',
      builtin: false,
      created_at: '',
      updated_at: '',
    },
  );
  const [busy, setBusy] = useState(false);

  const fnMatch = t.powershell.match(/^\s*function\s+([A-Za-z][A-Za-z0-9_-]*)/m);

  return (
    <>
      <h2>{initial ? `Edit step: ${initial.key}` : 'New step template'}</h2>
      <div className="toolbar">
        <input
          placeholder="key (lowercase, e.g. verify_checksum)"
          value={t.key}
          disabled={!!initial}
          onChange={(e) => setT({ ...t, key: e.target.value })}
        />
        <input
          placeholder="label shown in the flow builder"
          value={t.label}
          onChange={(e) => setT({ ...t, label: e.target.value })}
        />
        <button
          className="btn primary"
          disabled={busy || !t.key || !t.label}
          onClick={async () => {
            setBusy(true);
            await onSave(t, !initial);
            setBusy(false);
          }}
        >
          {busy ? 'Saving…' : initial ? 'Save changes' : 'Create'}
        </button>
        <button className="btn" onClick={onCancel}>
          Cancel
        </button>
      </div>

      <div className="card">
        <input
          style={{ width: '100%' }}
          placeholder="description (shown in the builder)"
          value={t.description}
          onChange={(e) => setT({ ...t, description: e.target.value })}
        />
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Parameters</h3>
        <p className="muted">
          Values are edited per-flow in the builder and reach your function as{' '}
          <code>$Params.&lt;key&gt;</code>.
        </p>
        {t.params.map((p, i) => (
          <div className="toolbar" key={i}>
            <input
              placeholder="key"
              value={p.key}
              onChange={(e) =>
                setT({ ...t, params: t.params.map((x, j) => (j === i ? { ...x, key: e.target.value } : x)) })
              }
            />
            <input
              placeholder="label"
              value={p.label}
              onChange={(e) =>
                setT({ ...t, params: t.params.map((x, j) => (j === i ? { ...x, label: e.target.value } : x)) })
              }
            />
            <input
              placeholder="placeholder / default hint"
              value={p.placeholder ?? ''}
              onChange={(e) =>
                setT({
                  ...t,
                  params: t.params.map((x, j) => (j === i ? { ...x, placeholder: e.target.value } : x)),
                })
              }
            />
            <button className="btn" onClick={() => setT({ ...t, params: t.params.filter((_, j) => j !== i) })}>
              ×
            </button>
          </div>
        ))}
        <button className="btn" onClick={() => setT({ ...t, params: [...t.params, { key: '', label: '' }] })}>
          Add parameter
        </button>
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>
          PowerShell {fnMatch ? <span className="muted">— defines {fnMatch[1]}</span> : ''}
        </h3>
        <p className="muted">
          Contract: <code>param([Parameter(Mandatory=$true)] [pscustomobject] $Job, [pscustomobject] $Params)</code>.
          Helpers available from EncodeLib: <code>Resolve-Tool</code>, <code>Invoke-Tool</code>,{' '}
          <code>Find-SourceFile</code>, <code>Assert-SafeName</code>. Emit{' '}
          <code>ENCODE_STEP &lt;key&gt; &lt;pct&gt;</code> for progress.
        </p>
        <textarea
          spellCheck={false}
          style={{ width: '100%', minHeight: 260, fontFamily: 'monospace', fontSize: 12 }}
          value={t.powershell}
          onChange={(e) => setT({ ...t, powershell: e.target.value })}
        />
      </div>
    </>
  );
}
