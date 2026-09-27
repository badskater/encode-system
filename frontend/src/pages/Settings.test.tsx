import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, it, vi, beforeEach, expect } from 'vitest';
import SettingsPage from './Settings';
import { api } from '../api/client';
import type { Settings, UpdateManifest } from '../types';

// A full Settings literal satisfying the merged interface (types.ts has two
// declarations). drain_mode defaults to false, matching the backend.
function baseSettings(overrides: Partial<Settings> = {}): Settings {
  return {
    controller_url: '',
    nfs_server: '',
    scripts_share: '',
    release_share: '',
    scripts_root: '/data/scripts',
    release_root: '/data/release',
    node_bin_dir: 'C:\\bin',
    node_scripts_dir: 'C:\\Encodes\\scripts',
    node_release_dir: 'C:\\Encodes\\ReleaseFolders',
    scan_interval_seconds: 30,
    tasks_before_reboot: 10,
    group: 'OldFartsSubs',
    tag: '1080p',
    discord_webhook: '',
    default_flow: '',
    ...overrides,
  };
}

// Manifest is independent of settings; stub a benign shape so publishing UI
// doesn't sit on a pending promise.
const manifest: UpdateManifest = {
  agent_version: '1.0.0',
  agent_sha256: '',
  lib_version: 1,
  lib_sha256: '',
  bin_version: 0,
  bin_sha256: '',
};

describe('Settings drain mode toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the toggle bound to settings.drain_mode (off by default)', async () => {
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/drain mode/i);
    expect(toggle).toBeInTheDocument();
    // defaults to false
    expect((toggle as HTMLInputElement).checked).toBe(false);
  });

  it('reflects settings.drain_mode=true as checked on load', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings({ drain_mode: true }));
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/drain mode/i);
    expect((toggle as HTMLInputElement).checked).toBe(true);
  });

  it('sends drain_mode=true in the PUT body after toggling on and saving', async () => {
    const saveSpy = vi
      .spyOn(api, 'saveSettings')
      .mockResolvedValue(baseSettings({ drain_mode: true }));
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/drain mode/i);
    // toggle on
    fireEvent.click(toggle);
    expect((toggle as HTMLInputElement).checked).toBe(true);
    // save — find the Save button inside the Behavior & naming card (the
    // toggle lives in the same card, so the nearest save applies to it).
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.drain_mode).toBe(true);
  });
});

describe('Settings hourly digest toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the digest toggle bound to settings.notify_digest (off by default)', async () => {
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/hourly digest/i);
    expect(toggle).toBeInTheDocument();
    // defaults to false (notify_digest absent in baseSettings)
    expect((toggle as HTMLInputElement).checked).toBe(false);
  });

  it('reflects settings.notify_digest=true as checked on load', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings({ notify_digest: true }));
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/hourly digest/i);
    expect((toggle as HTMLInputElement).checked).toBe(true);
  });

  it('sends notify_digest=true in the PUT body after toggling on and saving', async () => {
    const saveSpy = vi
      .spyOn(api, 'saveSettings')
      .mockResolvedValue(baseSettings({ notify_digest: true }));
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/hourly digest/i);
    fireEvent.click(toggle);
    expect((toggle as HTMLInputElement).checked).toBe(true);
    // The digest toggle lives in the Discord notifications card — the
    // second "Save settings" button belongs to that card.
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[1]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.notify_digest).toBe(true);
  });
});

describe('Settings job retention', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the retention field defaulting to 0 and sends the edited value on save', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings({ job_retention_days: 90 }));
    render(<SettingsPage />);
    const field = (await screen.findByLabelText(/job retention/i)) as HTMLInputElement;
    expect(field.value).toBe('0');

    fireEvent.change(field, { target: { value: '90' } });
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.job_retention_days).toBe(90);
  });

  it('clamps an out-of-range retention value into 0-3650 before saving', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    const field = (await screen.findByLabelText(/job retention/i)) as HTMLInputElement;
    fireEvent.change(field, { target: { value: '99999' } });
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.job_retention_days).toBe(3650);
  });
});

describe('Settings agent rollback', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
  });

  it('hides the rollback button when no previous release exists', async () => {
    vi.spyOn(api, 'manifest').mockResolvedValue({ ...manifest });
    render(<SettingsPage />);
    await screen.findByText(/push to nodes/i);
    expect(screen.queryByRole('button', { name: /rollback/i })).not.toBeInTheDocument();
  });

  it('shows and runs the rollback when a previous release exists', async () => {
    vi.spyOn(api, 'manifest').mockResolvedValue({
      ...manifest,
      agent_version: '1.1.0',
      prev_agent_version: '1.0.0',
    });
    const rollbackSpy = vi.spyOn(api, 'rollbackAgent').mockResolvedValue({
      ...manifest,
      agent_version: '1.0.0',
      prev_agent_version: '1.1.0',
    });
    render(<SettingsPage />);
    const btn = await screen.findByRole('button', { name: /rollback to 1\.0\.0/i });
    fireEvent.click(btn);
    await waitFor(() => expect(rollbackSpy).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(screen.getByText(/rolled back to agent 1\.0\.0/i)).toBeInTheDocument(),
    );
  });
});

describe('Settings disk alert threshold', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the disk alert field defaulting to 0 and sends the edited value on save', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings({ disk_alert_gb: 40 }));
    render(<SettingsPage />);
    const field = (await screen.findByLabelText(/disk alert/i)) as HTMLInputElement;
    expect(field.value).toBe('0');

    fireEvent.change(field, { target: { value: '40' } });
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.disk_alert_gb).toBe(40);
  });

  it('clamps an oversized disk alert value to 100000 before saving', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    const field = (await screen.findByLabelText(/disk alert/i)) as HTMLInputElement;
    fireEvent.change(field, { target: { value: '999999' } });
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.disk_alert_gb).toBe(100000);
  });
});
