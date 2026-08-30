import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import StepTimingsView from './StepTimingsView';
import type { StepTiming } from '../types';

function timing(step: string, duration_sec: number, started_at = '2026-01-02T03:04:05Z'): StepTiming {
  return { step, started_at, duration_sec };
}

describe('StepTimingsView', () => {
  it('renders nothing for an empty timings array', () => {
    const { container } = render(<StepTimingsView timings={[]} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders a row per step with a humanized duration', () => {
    render(
      <StepTimingsView
        timings={[
          timing('source_rename', 5),
          timing('encode', 7705), // 2h 08m 25s
          timing('mux', 45),
        ]}
      />,
    );
    expect(screen.getByText('source_rename')).toBeInTheDocument();
    expect(screen.getByText('encode')).toBeInTheDocument();
    expect(screen.getByText('mux')).toBeInTheDocument();
    // 5 seconds → "5s"
    expect(screen.getByText('5s')).toBeInTheDocument();
    // 45 seconds → "45s"
    expect(screen.getByText('45s')).toBeInTheDocument();
    // 7705s = 2h 08m 25s → "2h 08m"
    expect(screen.getByText('2h 08m')).toBeInTheDocument();
  });

  it('scales each bar width to the longest step (100%)', () => {
    const { container } = render(
      <StepTimingsView
        timings={[
          timing('a', 10),
          timing('b', 100),
          timing('c', 50),
        ]}
      />,
    );
    const bars = container.querySelectorAll<HTMLElement>('.step-bar-fill');
    expect(bars.length).toBe(3);
    // Longest (b=100) → 100%
    expect(bars[0].style.width).toBe('10%'); // a
    expect(bars[1].style.width).toBe('100%'); // b
    expect(bars[2].style.width).toBe('50%'); // c
  });

  it('handles a single step (bar is 100%)', () => {
    const { container } = render(<StepTimingsView timings={[timing('only', 30)]} />);
    const bar = container.querySelector<HTMLElement>('.step-bar-fill')!;
    expect(bar.style.width).toBe('100%');
  });
});
