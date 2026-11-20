import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import SeriesPage from './Series';
import { api } from '../api/client';
import { RoleContext } from '../hooks/useRole';
import type { Series, SeriesTemplate } from '../types';

// usePolling resolves fn() immediately on mount; mocking the api methods is
// enough to drive the page render without waiting for real timers.
function seriesFixture(overrides: Partial<Series> = {}): Series {
  return {
    id: 1,
    name: 'Show A',
    flow_id: 0,
    tag: '',
    enabled: true,
    jobs: 0,
    created_at: '2026-01-02T03:04:05Z',
    updated_at: '2026-01-02T03:04:05Z',
    ...overrides,
  };
}

function mockEmpty() {
  vi.spyOn(api, 'flows').mockResolvedValue([]);
  // The filter-script editor lists templates when it opens; keep the
  // endpoint mocked everywhere so no test hits an unmocked method.
  vi.spyOn(api, 'seriesTemplates').mockResolvedValue({ templates: [] });
  // api.settings is typed as the merged Settings interface; provide a full
  // fixture so TypeScript is satisfied across both declared shapes.
  vi.spyOn(api, 'settings').mockResolvedValue({
    controller_url: '',
    nfs_server: '',
    scripts_share: '',
    release_share: '',
    scripts_root: '',
    release_root: '',
    node_bin_dir: '',
    node_scripts_dir: '',
    node_release_dir: '',
    scan_interval_seconds: 0,
    tasks_before_reboot: 0,
    group: '',
    tag: 'default',
    discord_webhook: '',
    default_flow: '',
  });
}

describe('Series page progress column', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the Progress column header', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    expect(screen.getByText('Progress')).toBeInTheDocument();
  });

  it('renders "done/total" counts from mock data', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_done: 12, episodes_total: 24 }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('12/24')).toBeInTheDocument());

    expect(screen.getByText('12/24')).toBeInTheDocument();
  });

  it('scales the bar width to done/total %', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_done: 12, episodes_total: 24 }),
    ]);
    mockEmpty();

    const { container } = render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('12/24')).toBeInTheDocument());

    const bar = container.querySelector<HTMLElement>('.step-bar-fill')!;
    // 12/24 = 50%
    expect(bar.style.width).toBe('50%');
  });

  it('clamps the bar to 100% when done exceeds total', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_done: 30, episodes_total: 24 }),
    ]);
    mockEmpty();

    const { container } = render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('30/24')).toBeInTheDocument());

    const bar = container.querySelector<HTMLElement>('.step-bar-fill')!;
    expect(bar.style.width).toBe('100%');
  });

  it('shows failed and active muted counts when non-zero', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({
        episodes_done: 5,
        episodes_total: 24,
        episodes_failed: 3,
        episodes_active: 1,
      }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('5/24')).toBeInTheDocument());

    expect(screen.getByText('3 failed · 1 active')).toBeInTheDocument();
  });

  it('hides the failed/active line when all zero', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({
        episodes_done: 12,
        episodes_total: 24,
        episodes_failed: 0,
        episodes_active: 0,
      }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('12/24')).toBeInTheDocument());

    expect(screen.queryByText(/failed/)).not.toBeInTheDocument();
    expect(screen.queryByText(/active/)).not.toBeInTheDocument();
  });

  it('renders "no jobs yet" when episodes_total is 0', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ episodes_total: 0 }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('no jobs yet')).toBeInTheDocument());

    // No bar for a zero-total series.
    expect(document.querySelector('.step-bar-fill')).toBeNull();
    expect(screen.queryByText(/failed/)).not.toBeInTheDocument();
  });
});

describe('Series page notify mute toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders a Muted button for a series with notify=false', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: false }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    // Muted series shows the muted icon button.
    const muteBtn = screen.getByRole('button', { name: /unmute/i });
    expect(muteBtn).toBeInTheDocument();
    expect(muteBtn.textContent).toContain('Muted');
  });

  it('renders an active Alerts button for a series with notify=true (or absent)', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: true }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    const muteBtn = screen.getByRole('button', { name: /mute/i });
    expect(muteBtn).toBeInTheDocument();
    expect(muteBtn.textContent).toContain('Alerts');
  });

  it('PATCHes notify:false when muting an unmuted series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: true }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ notify: false }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /mute/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { notify: false }),
    );
  });

  it('PATCHes notify:true when unmuting a muted series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: false }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ notify: true }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /unmute/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { notify: true }),
    );
  });

  it('applies a subtle visual hint (opacity) to muted series names', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ notify: false }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    // The muted series name has reduced opacity.
    const nameCell = screen.getByText('Show A');
    expect(nameCell.style.opacity).toBe('0.7');
  });
});

describe('Series page pause toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders a Resume button for a paused series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: true }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    const pauseBtn = screen.getByRole('button', { name: /resume/i });
    expect(pauseBtn).toBeInTheDocument();
    expect(pauseBtn.textContent).toContain('Resume');
  });

  it('renders a Pause button for an unpaused series', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: false }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    const pauseBtn = screen.getByRole('button', { name: /^pause/i });
    expect(pauseBtn).toBeInTheDocument();
    expect(pauseBtn.textContent).toContain('Pause');
  });

  it('PATCHes paused:true when pausing', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: false }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ paused: true }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /^pause/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { paused: true }),
    );
  });

  it('PATCHes paused:false when resuming', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ paused: true }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ paused: false }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /resume/i }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { paused: false }),
    );
  });
});
describe('Series page node-group cell', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('shows the group when set and PATCHes node_group on edit', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ node_group: 'gpu' }),
    ]);
    mockEmpty();
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ node_group: 'cpu' }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('gpu')).toBeInTheDocument());

    fireEvent.click(screen.getByText('gpu'));
    const input = screen.getByPlaceholderText('any node');
    fireEvent.change(input, { target: { value: 'cpu' } });
    fireEvent.keyDown(input, { key: 'Enter' });

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { node_group: 'cpu' }),
    );
  });

  it('renders the "any node" hint when node_group is blank', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture({ node_group: '' })]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());
    expect(screen.getByText('any node')).toBeInTheDocument();
  });
});

describe('Series page webhook channel cell', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('shows global when no override and own when set', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ id: 1, name: 'A', webhook_url: '' }),
      seriesFixture({ id: 2, name: 'B', webhook_url: 'https://discord.com/api/webhooks/1/x' }),
    ]);
    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('A')).toBeInTheDocument());
    expect(screen.getByText('global')).toBeInTheDocument();
    expect(screen.getByText('📣 own')).toBeInTheDocument();
  });

  it('PATCHes webhook_url on save and rejects non-https silently', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture({ id: 1, name: 'A', webhook_url: '' })]);
    const patchSpy = vi.spyOn(api, 'patchSeries').mockResolvedValue(seriesFixture({ id: 1 }));
    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('global')).toBeInTheDocument());
    fireEvent.click(screen.getByText('global'));
    const input = screen.getByLabelText(/series webhook url/i);
    fireEvent.change(input, { target: { value: 'https://discord.com/api/webhooks/9/abc' } });
    fireEvent.keyDown(input, { key: 'Enter', code: 'Enter' });
    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { webhook_url: 'https://discord.com/api/webhooks/9/abc' }),
    );
    // non-https value must NOT be sent. The mocked api.series still
    // returns webhook_url:'' after refresh, so the cell shows "global".
    patchSpy.mockClear();
    await waitFor(() => expect(screen.getByText('global')).toBeInTheDocument());
    fireEvent.click(screen.getByText('global'));
    const input2 = screen.getByLabelText(/series webhook url/i);
    fireEvent.change(input2, { target: { value: 'http://evil.example/hook' } });
    fireEvent.keyDown(input2, { key: 'Enter', code: 'Enter' });
    expect(patchSpy).not.toHaveBeenCalled();
  });
});

describe('Series page filter-script cell', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  function templateFixture(name: string): SeriesTemplate {
    return { name, size: 123, mtime: '2026-11-20T21:00:00Z' };
  }

  it('shows the assigned template name on the series row', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ script_type: 'vpy', script_template: '1080.vpy' }),
    ]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());
    expect(screen.getByText('Filter script')).toBeInTheDocument();
    expect(screen.getByText('1080.vpy')).toBeInTheDocument();
  });

  it('shows a dash when no script is assigned', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  it('lists template names from seriesTemplates when the editor opens', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ script_type: 'vpy', script_template: '1080.vpy' }),
    ]);
    mockEmpty();
    const tplSpy = vi.spyOn(api, 'seriesTemplates').mockResolvedValue({
      templates: [templateFixture('1080.vpy'), templateFixture('2160.vpy')],
    });

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('1080.vpy')).toBeInTheDocument());
    fireEvent.click(screen.getByText('1080.vpy'));

    const picker = await screen.findByLabelText('Filter script template');
    expect(tplSpy).toHaveBeenCalledWith(1);
    await waitFor(() =>
      expect(screen.getByRole('option', { name: '2160.vpy' })).toBeInTheDocument(),
    );
    expect(screen.getByRole('option', { name: '1080.vpy' })).toBeInTheDocument();
    expect((picker as HTMLSelectElement).value).toBe('1080.vpy');
  });

  it('refreshes the template list when the type changes', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    const tplSpy = vi.spyOn(api, 'seriesTemplates').mockResolvedValue({
      templates: [templateFixture('1080.vpy'), templateFixture('1080.avs')],
    });

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('—')).toBeInTheDocument());
    fireEvent.click(screen.getByText('—'));
    const typeSelect = await screen.findByLabelText('Filter script type');
    // One fetch when the editor opens…
    await waitFor(() => expect(tplSpy).toHaveBeenCalledTimes(1));
    // …and one per type change.
    fireEvent.change(typeSelect, { target: { value: 'vpy' } });
    await waitFor(() => expect(tplSpy).toHaveBeenCalledTimes(2));
    fireEvent.change(typeSelect, { target: { value: 'avs' } });
    await waitFor(() => expect(tplSpy).toHaveBeenCalledTimes(3));
    expect(tplSpy).toHaveBeenLastCalledWith(1);
    // Switching type clears the stale template choice (extension mismatch).
    const picker = screen.getByLabelText('Filter script template') as HTMLSelectElement;
    expect(picker.value).toBe('');
  });

  it('PATCHes script_type and script_template together on save', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    vi.spyOn(api, 'seriesTemplates').mockResolvedValue({
      templates: [templateFixture('1080.vpy')],
    });
    const patchSpy = vi
      .spyOn(api, 'patchSeries')
      .mockResolvedValue(seriesFixture({ script_type: 'vpy', script_template: '1080.vpy' }));

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('—')).toBeInTheDocument());
    fireEvent.click(screen.getByText('—'));
    const typeSelect = await screen.findByLabelText('Filter script type');
    fireEvent.change(typeSelect, { target: { value: 'vpy' } });
    await waitFor(() =>
      expect(screen.getByRole('option', { name: '1080.vpy' })).toBeInTheDocument(),
    );
    fireEvent.change(screen.getByLabelText('Filter script template'), {
      target: { value: '1080.vpy' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, {
        script_type: 'vpy',
        script_template: '1080.vpy',
      }),
    );
  });

  it('PATCHes both fields empty when None is chosen', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ script_type: 'vpy', script_template: '1080.vpy' }),
    ]);
    mockEmpty();
    vi.spyOn(api, 'seriesTemplates').mockResolvedValue({
      templates: [templateFixture('1080.vpy')],
    });
    const patchSpy = vi.spyOn(api, 'patchSeries').mockResolvedValue(seriesFixture());

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('1080.vpy')).toBeInTheDocument());
    fireEvent.click(screen.getByText('1080.vpy'));
    const typeSelect = await screen.findByLabelText('Filter script type');
    fireEvent.change(typeSelect, { target: { value: '' } });
    // None hides the template picker entirely.
    expect(screen.queryByLabelText('Filter script template')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() =>
      expect(patchSpy).toHaveBeenCalledWith(1, { script_type: '', script_template: '' }),
    );
  });

  it('disables Save until a template is picked for a chosen type', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    vi.spyOn(api, 'seriesTemplates').mockResolvedValue({
      templates: [templateFixture('1080.vpy')],
    });
    const patchSpy = vi.spyOn(api, 'patchSeries').mockResolvedValue(seriesFixture());

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('—')).toBeInTheDocument());
    fireEvent.click(screen.getByText('—'));
    const typeSelect = await screen.findByLabelText('Filter script type');
    fireEvent.change(typeSelect, { target: { value: 'vpy' } });
    await waitFor(() =>
      expect(screen.getByRole('option', { name: '1080.vpy' })).toBeInTheDocument(),
    );
    // Half assignment (type without template) must not be sendable.
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    fireEvent.change(screen.getByLabelText('Filter script template'), {
      target: { value: '1080.vpy' },
    });
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
    expect(patchSpy).not.toHaveBeenCalled();
  });

  it('surfaces a 400 error message from the PATCH inline and keeps the editor open', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    vi.spyOn(api, 'seriesTemplates').mockResolvedValue({
      templates: [templateFixture('1080.vpy')],
    });
    vi.spyOn(api, 'patchSeries').mockRejectedValue(
      new Error('400: template "1080.vpy" not found under _templates/Show A/'),
    );

    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('—')).toBeInTheDocument());
    fireEvent.click(screen.getByText('—'));
    const typeSelect = await screen.findByLabelText('Filter script type');
    fireEvent.change(typeSelect, { target: { value: 'vpy' } });
    await waitFor(() =>
      expect(screen.getByRole('option', { name: '1080.vpy' })).toBeInTheDocument(),
    );
    fireEvent.change(screen.getByLabelText('Filter script template'), {
      target: { value: '1080.vpy' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() =>
      expect(
        screen.getByText('400: template "1080.vpy" not found under _templates/Show A/'),
      ).toBeInTheDocument(),
    );
    // Editor stays open so the operator can fix the assignment.
    expect(screen.getByLabelText('Filter script type')).toBeInTheDocument();
  });
});

describe('Series page delete action', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('shows a Delete action for admins and opens the confirm dialog with counts', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([
      seriesFixture({ jobs: 24, episodes_done: 12, episodes_total: 24 }),
    ]);
    mockEmpty();
    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: 'Delete Show A' }));
    // The confirm dialog shows the series' progress counts.
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByText('12/24 episodes encoded')).toBeInTheDocument();
    expect(screen.getByText(/jobs, history and files on the shares are kept/i)).toBeInTheDocument();
  });

  it('hides the Delete action (and Actions column) for viewers', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    render(
      <RoleContext.Provider value="viewer">
        <SeriesPage />
      </RoleContext.Provider>,
    );
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
    expect(screen.queryByText('Actions')).not.toBeInTheDocument();
  });

  it('confirm calls DELETE and refreshes the list; cancel does not', async () => {
    const listSpy = vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    const deleteSpy = vi.spyOn(api, 'deleteSeries').mockResolvedValue(undefined);
    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());

    // Cancel path: dialog closes, no API call.
    fireEvent.click(screen.getByRole('button', { name: 'Delete Show A' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(deleteSpy).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();

    // Confirm path: DELETE fires with the series id, dialog closes and the
    // list is re-fetched.
    const callsBefore = listSpy.mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: 'Delete Show A' }));
    fireEvent.click(screen.getByRole('button', { name: 'Delete series' }));
    await waitFor(() => expect(deleteSpy).toHaveBeenCalledWith(1));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await waitFor(() => expect(listSpy.mock.calls.length).toBeGreaterThan(callsBefore));
  });

  it('surfaces a delete failure in the dialog', async () => {
    vi.spyOn(api, 'series').mockResolvedValue([seriesFixture()]);
    mockEmpty();
    vi.spyOn(api, 'deleteSeries').mockRejectedValue(new Error('admin role required'));
    render(<SeriesPage />);
    await waitFor(() => expect(screen.getByText('Show A')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Delete Show A' }));
    fireEvent.click(screen.getByRole('button', { name: 'Delete series' }));
    await waitFor(() => expect(screen.getByText('admin role required')).toBeInTheDocument());
    // Dialog stays open so the operator sees the failure.
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });
});
