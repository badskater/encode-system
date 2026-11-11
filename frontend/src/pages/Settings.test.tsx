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
    linux_bin_dir: '',
    linux_scripts_dir: '',
    linux_release_dir: '',
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

describe('Settings linux node path overrides', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the three linux path fields empty by default and sends edited values on save', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    const binField = (await screen.findByLabelText(/linux bin dir/i)) as HTMLInputElement;
    const scriptsField = (await screen.findByLabelText(/linux scripts dir/i)) as HTMLInputElement;
    const releaseField = (await screen.findByLabelText(/linux release dir/i)) as HTMLInputElement;
    expect(binField.value).toBe('');
    expect(scriptsField.value).toBe('');
    expect(releaseField.value).toBe('');

    fireEvent.change(binField, { target: { value: '/opt/encode-bin' } });
    fireEvent.change(scriptsField, { target: { value: '/mnt/encode/scripts' } });
    fireEvent.change(releaseField, { target: { value: '/mnt/encode/release' } });
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.linux_bin_dir).toBe('/opt/encode-bin');
    expect(sent.linux_scripts_dir).toBe('/mnt/encode/scripts');
    expect(sent.linux_release_dir).toBe('/mnt/encode/release');
  });

  it('reflects persisted linux path values on load', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(
      baseSettings({
        linux_bin_dir: '/opt/encode-bin',
        linux_scripts_dir: '/mnt/encode/scripts',
        linux_release_dir: '/mnt/encode/release',
      }),
    );
    render(<SettingsPage />);
    const binField = (await screen.findByLabelText(/linux bin dir/i)) as HTMLInputElement;
    expect(binField.value).toBe('/opt/encode-bin');
  });
});


describe('Settings notify_node_status toggle', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the toggle bound to settings.notify_node_status (off by default)', async () => {
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/notify node status/i);
    expect(toggle).toBeInTheDocument();
    expect((toggle as HTMLInputElement).checked).toBe(false);
  });

  it('reflects settings.notify_node_status=true as checked on load', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings({ notify_node_status: true }));
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/notify node status/i);
    expect((toggle as HTMLInputElement).checked).toBe(true);
  });

  it('sends notify_node_status=true in the PUT body after toggling on and saving', async () => {
    const saveSpy = vi
      .spyOn(api, 'saveSettings')
      .mockResolvedValue(baseSettings({ notify_node_status: true }));
    render(<SettingsPage />);
    const toggle = await screen.findByLabelText(/notify node status/i);
    fireEvent.click(toggle);
    // The toggle lives in the Discord notifications card — second Save button.
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[1]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.notify_node_status).toBe(true);
  });
});

describe('Settings update check', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the version card with a Check for update button', async () => {
    render(<SettingsPage />);
    const btn = await screen.findByRole('button', { name: /check for update/i });
    expect(btn).toBeInTheDocument();
  });

  it('shows an available update notice when update_available is true', async () => {
    // GitHub's tag_name carries the leading "v" (real API shape) — the UI must
    // render exactly one "v" (regression: it once showed "vv1.1.0").
    vi.spyOn(api, 'checkUpdate').mockResolvedValue({
      current: 'v1.0.0',
      latest: 'v1.1.0',
      update_available: true,
      release_url: 'https://github.com/badskater/encode-system/releases/tag/v1.1.0',
    });
    render(<SettingsPage />);
    const btn = await screen.findByRole('button', { name: /check for update/i });
    fireEvent.click(btn);
    await waitFor(() => expect(screen.getByText(/v1\.1\.0 available/)).toBeInTheDocument());
    expect(screen.getByText(/Release notes/)).toBeInTheDocument();
    // Guard the exact defect: no doubled "v", and the upgrade command uses one "v".
    expect(screen.queryByText(/vv1\.1\.0/)).not.toBeInTheDocument();
    expect(screen.getByTestId('upgrade-cmd').textContent).toContain('bash -s v1.1.0');
    expect(screen.getByTestId('upgrade-cmd').textContent).not.toContain('vv1.1.0');
  });

  it('shows Up to date when no update is available', async () => {
    vi.spyOn(api, 'checkUpdate').mockResolvedValue({
      current: '1.1.0',
      latest: '1.1.0',
      update_available: false,
      release_url: '',
    });
    render(<SettingsPage />);
    const btn = await screen.findByRole('button', { name: /check for update/i });
    fireEvent.click(btn);
    await waitFor(() => expect(screen.getByText(/up to date/i)).toBeInTheDocument());
  });

  it('shows a muted Update check failed on a 502 error', async () => {
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(new Error('502: update check failed'));
    render(<SettingsPage />);
    const btn = await screen.findByRole('button', { name: /check for update/i });
    fireEvent.click(btn);
    await waitFor(() => expect(screen.getByText(/update check failed/i)).toBeInTheDocument());
  });
});

describe('Settings publish platform picker', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue({
      ...manifest,
      agent_version: '1.0.0',
      linux_agent_version: '0.9.0',
      prev_agent_version: '0.8.0',
      prev_linux_agent_version: '0.7.0',
    });
  });

  it('renders both Windows and Linux manifest rows', async () => {
    render(<SettingsPage />);
    await screen.findByText(/push to nodes/i);
    expect(screen.getByText('Windows agent')).toBeInTheDocument();
    expect(screen.getByText('Linux agent')).toBeInTheDocument();
  });

  it('defaults to Windows platform and shows the WIN button as active', async () => {
    render(<SettingsPage />);
    await screen.findByText(/push to nodes/i);
    const winBtn = screen.getByText('WIN').closest('button')!;
    expect(winBtn.getAttribute('aria-pressed')).toBe('true');
  });

  it('switches to Linux platform on click', async () => {
    render(<SettingsPage />);
    await screen.findByText(/push to nodes/i);
    const linuxBtn = screen.getByText('LINUX').closest('button')!;
    fireEvent.click(linuxBtn);
    expect(linuxBtn.getAttribute('aria-pressed')).toBe('true');
    // Agent version label now shows the platform
    expect(screen.getByText(/Agent version \(linux\)/i)).toBeInTheDocument();
  });

  it('shows the Windows rollback button when a Windows prev exists', async () => {
    render(<SettingsPage />);
    await screen.findByText(/push to nodes/i);
    expect(screen.getByRole('button', { name: /rollback to 0\.8\.0/i })).toBeInTheDocument();
  });

  it('shows the Linux rollback button after switching to Linux', async () => {
    render(<SettingsPage />);
    await screen.findByText(/push to nodes/i);
    fireEvent.click(screen.getByText('LINUX').closest('button')!);
    expect(screen.getByRole('button', { name: /rollback to 0\.7\.0/i })).toBeInTheDocument();
  });
});

describe('Settings stall timeout', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  it('renders the stall timeout field bound to settings (0 = off by default)', async () => {
    render(<SettingsPage />);
    const input = (await screen.findByLabelText(/stall timeout/i)) as HTMLInputElement;
    expect(input).toBeInTheDocument();
    expect(input.value).toBe('0');
  });

  it('reflects a loaded stall_timeout_minutes value', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings({ stall_timeout_minutes: 45 }));
    render(<SettingsPage />);
    const input = (await screen.findByLabelText(/stall timeout/i)) as HTMLInputElement;
    expect(input.value).toBe('45');
  });

  it('sends stall_timeout_minutes in the PUT body after editing and saving', async () => {
    const saveSpy = vi
      .spyOn(api, 'saveSettings')
      .mockResolvedValue(baseSettings({ stall_timeout_minutes: 60 }));
    render(<SettingsPage />);
    const input = await screen.findByLabelText(/stall timeout/i);
    fireEvent.change(input, { target: { value: '60' } });
    // The field lives in the Behavior & naming card — its Save button is first.
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.stall_timeout_minutes).toBe(60);
  });

  it('clamps an out-of-range stall timeout to the 0-10080 bound on save', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    const input = await screen.findByLabelText(/stall timeout/i);
    fireEvent.change(input, { target: { value: '99999' } });
    const saveButtons = await screen.findAllByRole('button', { name: /save settings/i });
    fireEvent.click(saveButtons[0]);
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Settings;
    expect(sent.stall_timeout_minutes).toBe(10080);
  });
});

describe('Settings notification channels card', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'settings').mockResolvedValue(baseSettings());
    vi.spyOn(api, 'manifest').mockResolvedValue(manifest);
  });

  // The channels card's Save button is the LAST "Save settings" on the page
  // (Behavior & naming and Discord notifications render theirs first).
  async function channelsSaveButton() {
    const buttons = await screen.findAllByRole('button', { name: /save settings/i });
    return buttons[buttons.length - 1];
  }

  it('renders the channel fields and write-only secret inputs', async () => {
    render(<SettingsPage />);
    expect(await screen.findByLabelText(/generic webhook url/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/ntfy topic url/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/smtp host/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/smtp port/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/smtp from/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/smtp to/i)).toBeInTheDocument();
    const tok = screen.getByLabelText(/ntfy access token/i) as HTMLInputElement;
    const pw = screen.getByLabelText(/smtp password/i) as HTMLInputElement;
    expect(tok.type).toBe('password');
    expect(pw.type).toBe('password');
    // Never prefilled — the API never returns secret values.
    expect(tok.value).toBe('');
    expect(pw.value).toBe('');
  });

  it('shows the saved placeholder when has_* flags are set', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(
      baseSettings({ has_ntfy_token: true, has_smtp_password: true }),
    );
    render(<SettingsPage />);
    const tok = (await screen.findByLabelText(/ntfy access token/i)) as HTMLInputElement;
    expect(tok.placeholder).toContain('saved');
    expect(tok.value).toBe('');
    const pw = screen.getByLabelText(/smtp password/i) as HTMLInputElement;
    expect(pw.placeholder).toContain('saved');
    expect(pw.value).toBe('');
  });

  it('sends typed secrets once and clears the inputs after save', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(
      baseSettings({ has_ntfy_token: true, has_smtp_password: true }),
    );
    render(<SettingsPage />);
    const tok = (await screen.findByLabelText(/ntfy access token/i)) as HTMLInputElement;
    const pw = screen.getByLabelText(/smtp password/i) as HTMLInputElement;
    // Assembled at runtime: fake values, redaction-safe.
    fireEvent.change(tok, { target: { value: 'tok-' + '123' } });
    fireEvent.change(pw, { target: { value: 'pw-' + '456' } });
    fireEvent.click(await channelsSaveButton());
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0];
    expect(sent.ntfy_token).toBe('tok-' + '123');
    expect(sent.smtp_password).toBe('pw-' + '456');
    // Consumed secrets are cleared so the next save keeps them.
    await waitFor(() => expect(tok.value).toBe(''));
    expect(pw.value).toBe('');
  });

  it('omits secret keys when nothing was typed (keep stored values)', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.click(await channelsSaveButton());
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0];
    expect(sent.ntfy_token).toBeUndefined();
    expect(sent.smtp_password).toBeUndefined();
  });

  it('strips the GET-only has_* flags from the PUT payload', async () => {
    vi.spyOn(api, 'settings').mockResolvedValue(
      baseSettings({ has_ntfy_token: true, has_smtp_password: true }),
    );
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.click(await channelsSaveButton());
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0] as Record<string, unknown>;
    expect(sent.has_ntfy_token).toBeUndefined();
    expect(sent.has_smtp_password).toBeUndefined();
  });

  it('sends channel URLs and SMTP fields in the PUT body', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/generic webhook url/i), {
      target: { value: 'https://hooks.example.com/x' },
    });
    fireEvent.change(screen.getByLabelText(/ntfy topic url/i), {
      target: { value: 'https://ntfy.sh/encode' },
    });
    fireEvent.change(screen.getByLabelText(/smtp host/i), { target: { value: 'mail.example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp from/i), { target: { value: 'encode@example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp to/i), { target: { value: 'ops@example.com' } });
    fireEvent.click(await channelsSaveButton());
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0];
    expect(sent.generic_webhook_url).toBe('https://hooks.example.com/x');
    expect(sent.ntfy_topic_url).toBe('https://ntfy.sh/encode');
    expect(sent.smtp_host).toBe('mail.example.com');
    expect(sent.smtp_from).toBe('encode@example.com');
    expect(sent.smtp_to).toBe('ops@example.com');
    expect(sent.smtp_port).toBe(0);
  });

  it('blocks save on an invalid generic webhook URL', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/generic webhook url/i), {
      target: { value: 'notaurl' },
    });
    fireEvent.click(await channelsSaveButton());
    expect(await screen.findByText(/generic webhook url must be an http/i)).toBeInTheDocument();
    expect(saveSpy).not.toHaveBeenCalled();
  });

  it('blocks save on an out-of-range SMTP port', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/smtp port/i), { target: { value: '70000' } });
    fireEvent.click(await channelsSaveButton());
    expect(await screen.findByText(/smtp port must be/i)).toBeInTheDocument();
    expect(saveSpy).not.toHaveBeenCalled();
  });

  it('blocks save when SMTP host is set without from/to', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/smtp host/i), {
      target: { value: 'mail.example.com' },
    });
    fireEvent.click(await channelsSaveButton());
    expect(await screen.findByText(/smtp from and to are required/i)).toBeInTheDocument();
    expect(saveSpy).not.toHaveBeenCalled();
  });

  it('blocks save on an invalid SMTP recipient list', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/smtp host/i), { target: { value: 'mail.example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp from/i), { target: { value: 'encode@example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp to/i), { target: { value: 'ops@example.com, bad' } });
    fireEvent.click(await channelsSaveButton());
    expect(await screen.findByText(/smtp to must be comma-separated/i)).toBeInTheDocument();
    expect(saveSpy).not.toHaveBeenCalled();
  });

  // G6: the client must accept the same address shapes as the server's
  // net/mail.ParseAddress — notably dotless domains (ops@localhost), which
  // the old dot-requiring regex rejected while the server accepted them.
  it('accepts dotless-domain SMTP addresses like the server (ops@localhost)', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/smtp host/i), { target: { value: 'mail.example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp from/i), { target: { value: 'ops@localhost' } });
    fireEvent.change(screen.getByLabelText(/smtp to/i), { target: { value: 'oncall@localhost' } });
    fireEvent.click(await channelsSaveButton());
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    const sent = saveSpy.mock.calls[0][0];
    expect(sent.smtp_from).toBe('ops@localhost');
    expect(sent.smtp_to).toBe('oncall@localhost');
  });

  it('accepts a display-name form with a dotless domain', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/smtp host/i), { target: { value: 'mail.example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp from/i), { target: { value: 'Encode Bot <ops@localhost>' } });
    fireEvent.change(screen.getByLabelText(/smtp to/i), { target: { value: 'ops@example.com' } });
    fireEvent.click(await channelsSaveButton());
    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
    expect(saveSpy.mock.calls[0][0].smtp_from).toBe('Encode Bot <ops@localhost>');
  });

  it('still blocks genuinely invalid SMTP addresses', async () => {
    const saveSpy = vi.spyOn(api, 'saveSettings').mockResolvedValue(baseSettings());
    render(<SettingsPage />);
    fireEvent.change(await screen.findByLabelText(/smtp host/i), { target: { value: 'mail.example.com' } });
    fireEvent.change(screen.getByLabelText(/smtp from/i), { target: { value: 'not-an-email' } });
    fireEvent.change(screen.getByLabelText(/smtp to/i), { target: { value: 'ops@example.com' } });
    fireEvent.click(await channelsSaveButton());
    expect(await screen.findByText(/smtp from must be a valid email/i)).toBeInTheDocument();
    expect(saveSpy).not.toHaveBeenCalled();
  });
});
