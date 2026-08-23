import { useState } from 'react';
import { api } from '../api/client';
import type { Flow, Step } from '../types';
import { usePolling } from '../hooks/usePolling';
import { stepMeta } from '../types';
import FlowBuilder from '../components/FlowBuilder';

// FlowsPage lists flows and hosts the visual builder for create/edit.
export default function FlowsPage() {
  const { data: flows, error } = usePolling<Flow[]>(() => api.flows(), 30000);
  const [editing, setEditing] = useState<Flow | null>(null);
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  async function save(name: string, steps: Step[]) {
    if (editing) {
      await api.updateFlow(editing.id, { name, steps });
    } else {
      await api.createFlow({ name, steps });
    }
    setEditing(null);
    setCreating(false);
  }

  async function remove(f: Flow) {
    try {
      await api.deleteFlow(f.id);
      setActionError(null);
    } catch (e) {
      setActionError(String(e));
    }
  }

  if (creating || editing) {
    return (
      <>
        <h2>{editing ? `Edit flow: ${editing.name}` : 'New flow'}</h2>
        <FlowBuilder
          initial={editing}
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

      <div className="toolbar">
        <button className="btn primary" onClick={() => setCreating(true)}>
          New flow
        </button>
      </div>

      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Pipeline</th>
            <th>Steps</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(flows ?? []).map((f) => (
            <tr key={f.id}>
              <td>{f.name}</td>
              <td className="muted">
                {f.steps.map((s) => stepMeta(s.type).label).join(' → ')}
              </td>
              <td>{f.steps.length}</td>
              <td>
                <button className="btn" onClick={() => setEditing(f)}>
                  Edit
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
