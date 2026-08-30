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
