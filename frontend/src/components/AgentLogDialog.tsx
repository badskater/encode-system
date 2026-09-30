// AgentLogDialog shows the tail of a node's own agent log as shipped in its
// latest heartbeat (nodes.agent_log, ≤8 KiB, newest last). No extra fetch —
// the Nodes list already carries the field, so the dialog renders what it is
// given and pretty-prints JSON slog lines where parseable. Uses the same
// modal-backdrop/card-modal pattern (and job-log-pre styling) as
// JobLogDialog so both log viewers read consistently.
import { formatAgentLog } from './helpers';

export default function AgentLogDialog({
  nodeName,
  log,
  onClose,
}: {
  nodeName: string;
  log: string;
  onClose: () => void;
}) {
  return (
    <div className="modal-backdrop" onClick={onClose} role="presentation">
      <div
        className="card modal"
        style={{ maxWidth: 760 }}
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label={`Agent log for ${nodeName}`}
      >
        <h3>Agent log — {nodeName}</h3>
        <p className="muted" style={{ fontSize: 12 }}>
          Last lines the agent shipped in its heartbeat (newest at the bottom).
        </p>
        <pre className="job-log-pre">{formatAgentLog(log)}</pre>
        <div className="toolbar">
          <button className="btn" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
}
