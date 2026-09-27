import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import JobMetricsView from './JobMetricsView';

describe('JobMetricsView', () => {
  it('renders nothing when metrics absent or empty', () => {
    const { container, rerender } = render(<JobMetricsView />);
    expect(container.firstChild).toBeNull();
    rerender(<JobMetricsView metrics={{}} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders known keys with friendly labels and formatting', () => {
    render(<JobMetricsView metrics={{ vmaf: 94.214, output_bitrate_kbps: 8123, duration_sec: 1440.5 }} />);
    expect(screen.getByText('VMAF')).toBeInTheDocument();
    expect(screen.getByText('94.21')).toBeInTheDocument();
    expect(screen.getByText('Bitrate')).toBeInTheDocument();
    expect(screen.getByText('8,123 kb/s')).toBeInTheDocument();
    expect(screen.getByText('24m 01s')).toBeInTheDocument();
  });

  it('flags low VMAF with the warn class', () => {
    const { container } = render(<JobMetricsView metrics={{ vmaf: 85.1 }} />);
    expect(container.querySelector('.chip.warn')).not.toBeNull();
  });

  it('does not flag healthy VMAF', () => {
    const { container } = render(<JobMetricsView metrics={{ vmaf: 95.5 }} />);
    expect(container.querySelector('.chip.warn')).toBeNull();
  });

  it('humanizes unknown keys and renders raw values', () => {
    render(<JobMetricsView metrics={{ custom_thing: 12 }} />);
    expect(screen.getByText('Custom thing')).toBeInTheDocument();
    expect(screen.getByText('12')).toBeInTheDocument();
  });

  it('drops non-finite values instead of rendering NaN', () => {
    const { container } = render(
      <JobMetricsView metrics={{ vmaf: Number.NaN, output_size_mb: 1024.25 }} />,
    );
    expect(container.textContent).not.toContain('NaN');
    expect(screen.getByText('Out size')).toBeInTheDocument();
  });

  it('orders known keys before unknown ones', () => {
    render(<JobMetricsView metrics={{ zz_unknown: 1, vmaf: 93 }} />);
    const labels = screen.getAllByText(/.+/).map((el) => el.textContent);
    const vi = labels.indexOf('VMAF');
    const ui = labels.indexOf('Zz unknown');
    expect(vi).toBeGreaterThanOrEqual(0);
    expect(ui).toBeGreaterThanOrEqual(0);
    expect(vi).toBeLessThan(ui);
  });
});
