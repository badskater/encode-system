import { useRef, useState } from 'react';
import { api } from '../api/client';
import type { Flow, FlowExport, Step } from '../types';
import { usePolling } from '../hooks/usePolling';
import FlowBuilder from '../components/FlowBuilder';

// FlowsPage lists saved flow sequences, marks the default flow, and hosts the
// visual builder plus JSON import/export. Multiple flows can coexist; the
// default is used when a series has no explicit flow selection.
export default function FlowsPage() {
  const { data: flows, error } = usePolling<Flow[]>(() => api.flows(), 30000);
  const { data: templates } = usePolling(() => api.stepTemplates(), 30000);
  const [editing, setEditing] = useState<Flow | null>(null);
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  async function save(name: string, steps: Step[]) {
    if (editing) {
      await api.updateFlow(editing.id, { name, steps });
    } else {
      await api.createFlow({ name, steps, is_default: false });
    }
    setEditing(null);
    setCreating(false);
  }

  async function remove(f: Flow) {
    try {
      await api.deleteFlow(f.id);
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  async function makeDefault(f: Flow) {
    try {
      await api.setDefaultFlow(f.id);
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  async function exportFlow(f: Flow) {
    try {
      const exp = await api.exportFlow(f.id);
      const blob = new Blob([JSON.stringify(exp, null, 2)], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `flow-${f.name}.json`;
      a.click();
      URL.revokeObjectURL(url);
      setActionError(null);
    } catch (e) {
      setActionError(String(e instanceof Error ? e.message : e));
    }
  }

  async function importFlow(file: File) {
    try {
      const text = await file.text();
      const payload = JSON.parse(text) as FlowExport;
      await api.importFlow(payload);
      setActionError(null);
    } catch (e) {
      setActionError(`import failed: ${e instanceof Error ? e.message : e}`);
    }
  }

  if (creating || editing) {
    return (
      <>
        <h2>{editing ? `Edit flow: ${editing.name}` : 'New flow'}</h2>
        <FlowBuilder
          initial={editing}
          templates={templates ?? []}
          onSave={save}
          onCancel={() => {
            setEditing(null);
            setCreating(false);
          }}
        />
      </>
    );
  }

  return (
    <>
      <h2>Flows</h2>
      {error && <div className="error-box">{error}</div>}
      {actionError && <div className="error-box">{actionError}</div>}

      <p className="muted">
        Save multiple encode sequences. The flow marked <span className="badge blue">default</span>{' '}
        is used for series without their own selection; any series can pick its own flow on the
        Series page. Export a flow to share it — the JSON embeds any custom step templates it uses.
      </p>

      <div className="toolbar">
        <button className="btn primary" onClick={() => setCreating(true)}>
          New flow
        </button>
        <button className="btn" onClick={() => fileRef.current?.click()}>
          Import JSON
        </button>
        <input
          ref={fileRef}
          type="file"
          accept=".json,application/json"
          style={{ display: 'none' }}
          onChange={(e) => {
            const f = e.target.files?.[0];
            if (f) importFlow(f);
            e.target.value = '';
          }}
        />
      </div>

      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Pipeline</th>
            <th>Steps</th>
            <th>Default</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(flows ?? []).map((f) => (
            <tr key={f.id}>
              <td>{f.name}</td>
              <td className="muted">
                {f.steps
                  .map((s) => {
                    const t = (templates ?? []).find((x) => x.key === s.type);
                    return t ? t.label : s.type;
                  })
                  .join(' → ')}
              </td>
              <td>{f.steps.length}</td>
              <td>
                {f.is_default ? (
                  <span className="badge blue">default</span>
                ) : (
                  <button className="btn" onClick={() => makeDefault(f)}>
                    Make default
                  </button>
                )}
              </td>
              <td>
                <button className="btn" onClick={() => setEditing(f)}>
                  Edit
                </button>{' '}
                <button className="btn" onClick={() => exportFlow(f)}>
                  Export
                </button>{' '}
                <button className="btn danger" onClick={() => remove(f)}>
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
