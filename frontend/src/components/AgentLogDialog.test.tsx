import { render, screen, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import AgentLogDialog from './AgentLogDialog';
import { formatAgentLog } from './helpers';

// AgentLogDialog renders the heartbeat-shipped agent log tail (nodes.agent_log)
// in the shared modal pattern. formatAgentLog pretty-prints JSON slog lines
// and passes non-JSON through untouched.

describe('formatAgentLog', () => {
  it('pretty-prints a JSON slog line into LEVEL time msg k=v form', () => {
    const line = JSON.stringify({
      time: '2026-09-29T12:34:56Z',
      level: 'WARN',
      msg: 'swap failed',
      err: 'access denied',
      job: 42,
    });
    const out = formatAgentLog(line);
    expect(out).toContain('WARN');
    expect(out).toContain('swap failed');
    expect(out).toContain('err=access denied');
    expect(out).toContain('job=42');
    // The raw JSON braces are gone — the line was parsed, not passed through.
    expect(out).not.toContain('{');
  });

  it('passes non-JSON lines through untouched', () => {
    const raw = 'powershell wrote this\nnot json at all';
    expect(formatAgentLog(raw)).toBe(raw);
  });

  it('returns an explanatory placeholder for an empty log', () => {
    expect(formatAgentLog('')).toContain('no agent log reported');
  });
});

describe('AgentLogDialog', () => {
  it('renders the node name and formatted log content', () => {
    const log = JSON.stringify({ time: '2026-09-29T12:00:00Z', level: 'INFO', msg: 'heartbeat ok' });
    render(<AgentLogDialog nodeName="enc-01" log={log} onClose={() => {}} />);

    expect(screen.getByText(/Agent log — enc-01/)).toBeInTheDocument();
    expect(screen.getByText(/heartbeat ok/)).toBeInTheDocument();
  });

  it('closes when the Close button is clicked', () => {
    const onClose = vi.fn();
    render(<AgentLogDialog nodeName="enc-01" log="x" onClose={onClose} />);

    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes when the backdrop is clicked but not the dialog body', () => {
    const onClose = vi.fn();
    render(<AgentLogDialog nodeName="enc-01" log="x" onClose={onClose} />);

    // Click inside the dialog body — stopPropagation must keep it open.
    fireEvent.click(screen.getByRole('dialog'));
    expect(onClose).not.toHaveBeenCalled();

    // Backdrop click closes.
    fireEvent.click(screen.getByRole('dialog').parentElement!);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
