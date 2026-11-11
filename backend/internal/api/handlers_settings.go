package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"net/url"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Settings (NFS shares, roots, remote path mapping) ----------

// settingsReq is the PUT request body. model.Settings hides the write-only
// secrets (json:"-"), so the handler decodes into this shadow struct to read
// them IN (same pattern as shareReq). The has_* output flags are accepted
// and ignored so a verbatim GET→PUT round-trip keeps working.
type settingsReq struct {
	model.Settings
	NtfyToken       string `json:"ntfy_token,omitempty"`
	SMTPPassword    string `json:"smtp_password,omitempty"`
	HasNtfyToken    bool   `json:"has_ntfy_token,omitempty"`    // output-only echo, ignored
	HasSMTPPassword bool   `json:"has_smtp_password,omitempty"` // output-only echo, ignored
}

// settingsOut is the API-facing settings: identical to model.Settings plus
// has_* flags for the write-only secrets (plaintext never leaves the
// controller — same mask pattern as shareOut).
type settingsOut struct {
	*model.Settings
	HasNtfyToken    bool `json:"has_ntfy_token"`
	HasSMTPPassword bool `json:"has_smtp_password"`
}

// MarshalJSON implements the mask: embed Settings' fields (secrets already
// json:"-" on the model) and add the has_* flags.
func (o settingsOut) MarshalJSON() ([]byte, error) {
	type alias model.Settings
	return json.Marshal(struct {
		alias
		HasNtfyToken    bool `json:"has_ntfy_token"`
		HasSMTPPassword bool `json:"has_smtp_password"`
	}{alias(*o.Settings), o.HasNtfyToken, o.HasSMTPPassword})
}

func maskSettings(st *model.Settings) settingsOut {
	return settingsOut{Settings: st, HasNtfyToken: st.NtfyToken != "", HasSMTPPassword: st.SMTPPassword != ""}
}

// defaultsSettings builds the settings baseline from the environment (the
// values the controller booted with). Used as the merge source whenever no
// database row exists yet, and as the template for PUT responses.
func (s *Server) defaultsSettings() *model.Settings {
	return &model.Settings{
		ControllerURL:       "", // operator sets the externally-reachable URL
		ScriptsRoot:         s.Cfg.ScriptsRoot,
		ReleaseRoot:         s.Cfg.ReleaseRoot,
		NodeBinDir:          s.Cfg.NodeBinDir,
		NodeScriptsDir:      s.Cfg.NodeScriptsDir,
		NodeReleaseDir:      s.Cfg.NodeReleaseDir,
		ScanIntervalSeconds: s.Cfg.ScanIntervalSeconds,
		TasksBeforeReboot:   s.Cfg.TasksBeforeReboot,
		Group:               s.Cfg.Group,
		Tag:                 s.Cfg.Tag,
		DiscordWebhook:      s.Cfg.DiscordWebhook,
	}
}

// currentSettings returns the effective settings: the persisted row when it
// exists, otherwise the environment defaults. Never nil. Callers that run on
// hot paths (heartbeat/job rendering) read this every time so UI edits apply
// without a controller restart.
func (s *Server) currentSettings(ctx context.Context) *model.Settings {
	st, err := s.Store.GetSettings(ctx)
	if err != nil {
		// Distinguish "no row yet" (nil, nil) from a real failure: a corrupt
		// row or a broken DB must be visible, or jobs would silently render
		// with env defaults instead of the operator's configured paths.
		s.Log.Warn("settings load failed, falling back to env defaults", "err", err)
	}
	if st != nil {
		return st
	}
	return s.defaultsSettings()
}

// handleGetSettings exposes the live settings (NFS share config, controller
// roots, remote path mapping, behavior) to the UI. Write-only secrets are
// masked to has_* flags.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, maskSettings(s.currentSettings(r.Context())))
}

// handleUpdateSettings replaces the settings row. Validation keeps the
// operator from shooting themselves in the foot: paths must be absolute on
// their respective OS, intervals sane, required fields present.
//
// Write-only secrets (ntfy_token, smtp_password) follow the shares pattern:
// an EMPTY value on PUT keeps the stored credential (the API never echoes
// plaintext, so the UI cannot round-trip it); a non-empty value replaces it.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	st := req.Settings
	st.NtfyToken = req.NtfyToken
	st.SMTPPassword = req.SMTPPassword
	if err := validateSettings(&st); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Keep-semantics for write-only secrets: blank = keep the stored value.
	// Read the effective settings BEFORE the save so the merge sees the
	// current row (or env defaults when no row exists yet).
	prev := s.currentSettings(r.Context())
	if st.NtfyToken == "" {
		st.NtfyToken = prev.NtfyToken
	}
	if st.SMTPPassword == "" {
		st.SMTPPassword = prev.SMTPPassword
	}
	if err := s.Store.SaveSettings(r.Context(), &st); err != nil {
		writeErr(w, http.StatusInternalServerError, "save settings")
		return
	}
	s.Log.Info("settings updated via UI", "scripts_root", st.ScriptsRoot,
		"node_bin_dir", st.NodeBinDir, "scan_interval_s", st.ScanIntervalSeconds)
	// Audit a secret-free subset: webhook URLs and credential fields must
	// never land in the audit table — only the operational knobs plus
	// booleans for whether each channel/secret is set or changed.
	s.audit(r, "settings.update", "settings", map[string]any{
		"scan_interval_seconds": st.ScanIntervalSeconds,
		"tasks_before_reboot":   st.TasksBeforeReboot,
		"drain_mode":            st.DrainMode,
		"notify_digest":         st.NotifyDigest,
		"notify_node_status":    st.NotifyNodeStatus,
		"job_retention_days":    st.JobRetentionDays,
		"disk_alert_gb":         st.DiskAlertGB,
		"stall_timeout_minutes": st.StallTimeoutMinutes,
		"webhook_set":           st.DiscordWebhook != "",
		"generic_webhook_set":   st.GenericWebhookURL != "",
		"ntfy_set":              st.NtfyTopicURL != "",
		"smtp_set":              st.SMTPHost != "",
		"ntfy_token_changed":    req.NtfyToken != "",
		"smtp_password_changed": req.SMTPPassword != "",
	})
	writeJSON(w, http.StatusOK, maskSettings(s.currentSettings(r.Context())))
}

// validateSettings checks invariants the renderer/scanner depend on.
func validateSettings(st *model.Settings) error {
	if st.ScriptsRoot == "" {
		return errSettings("scripts_root is required")
	}
	if st.ReleaseRoot == "" {
		return errSettings("release_root is required")
	}
	if st.NodeBinDir == "" {
		return errSettings("node_bin_dir is required (tools folder on nodes)")
	}
	if st.NodeScriptsDir == "" {
		return errSettings("node_scripts_dir is required (scripts mount on nodes)")
	}
	if st.NodeReleaseDir == "" {
		return errSettings("node_release_dir is required (release mount on nodes)")
	}
	if st.ScanIntervalSeconds < 5 {
		return errSettings("scan_interval_seconds must be >= 5")
	}
	if st.ScanIntervalSeconds > 3600 {
		return errSettings("scan_interval_seconds must be <= 3600")
	}
	if st.TasksBeforeReboot < 1 || st.TasksBeforeReboot > 1000 {
		return errSettings("tasks_before_reboot must be 1-1000")
	}
	// Retention: 0 disables pruning (keep forever); anything else must be
	// at least a day and at most ~10 years. Negative or absurd values would
	// either delete everything on the next tick or never match.
	if st.JobRetentionDays < 0 || st.JobRetentionDays > 3650 {
		return errSettings("job_retention_days must be 0 (keep forever) or 1-3650")
	}
	// Disk alert: 0 disables the check; otherwise 1 GB - 100 TB is the
	// sane range for a threshold on an encode farm.
	if st.DiskAlertGB < 0 || st.DiskAlertGB > 100000 {
		return errSettings("disk_alert_gb must be 0 (disabled) or 1-100000")
	}
	// Stall watchdog: 0 disables it; otherwise 1 minute - 1 week. The
	// threshold must exceed the longest interval WITHOUT step/progress
	// change in any healthy job (e.g. a long encode that only marks start
	// and finish) — not the heartbeat cadence: heartbeats reporting an
	// unchanged step/progress deliberately do NOT refresh the stall stamp.
	if st.StallTimeoutMinutes < 0 || st.StallTimeoutMinutes > 10080 {
		return errSettings("stall_timeout_minutes must be 0 (disabled) or 1-10080")
	}
	if st.Group == "" {
		return errSettings("group is required")
	}
	if st.Tag == "" {
		return errSettings("tag is required")
	}
	// Discord webhook: optional (blank = notifications off). When set it must
	// look like a Discord webhook URL (loopback allowed for local mock
	// testing) — mirroring the discord_notify step's own guard so a typo or
	// an exfiltration target is caught at save time, not mid-job.
	st.DiscordWebhook = strings.TrimSpace(st.DiscordWebhook)
	if st.DiscordWebhook != "" {
		discordish := strings.HasPrefix(st.DiscordWebhook, "https://discord.com/api/webhooks/") ||
			strings.HasPrefix(st.DiscordWebhook, "https://discordapp.com/api/webhooks/")
		loopback := strings.HasPrefix(st.DiscordWebhook, "http://localhost") ||
			strings.HasPrefix(st.DiscordWebhook, "https://localhost") ||
			strings.HasPrefix(st.DiscordWebhook, "http://127.0.0.1") ||
			strings.HasPrefix(st.DiscordWebhook, "https://127.0.0.1")
		if !discordish && !loopback {
			return errSettings("discord_webhook must be a Discord webhook URL (https://discord.com/api/webhooks/...) or blank")
		}
	}
	// Generic webhook + ntfy topic: optional (blank = channel disabled).
	// When set they must be absolute http(s) URLs — the controller POSTs to
	// them, so a typo or a non-HTTP scheme is caught at save time.
	st.GenericWebhookURL = strings.TrimSpace(st.GenericWebhookURL)
	if st.GenericWebhookURL != "" && !isHTTPURL(st.GenericWebhookURL) {
		return errSettings("generic_webhook_url must be an http(s) URL or blank")
	}
	st.NtfyTopicURL = strings.TrimSpace(st.NtfyTopicURL)
	if st.NtfyTopicURL != "" && !isHTTPURL(st.NtfyTopicURL) {
		return errSettings("ntfy_topic_url must be an http(s) URL (e.g. https://ntfy.sh/mytopic) or blank")
	}
	// SMTP email channel: enabled when host+from+to are all set. Port 0
	// defaults to 587 at send time; anything else must be a real port.
	st.SMTPHost = strings.TrimSpace(st.SMTPHost)
	st.SMTPUser = strings.TrimSpace(st.SMTPUser)
	st.SMTPFrom = strings.TrimSpace(st.SMTPFrom)
	st.SMTPTo = strings.TrimSpace(st.SMTPTo)
	if st.SMTPPort < 0 || st.SMTPPort > 65535 {
		return errSettings("smtp_port must be 0 (default 587) or 1-65535")
	}
	if st.SMTPHost != "" && (strings.ContainsAny(st.SMTPHost, " 	") || strings.Contains(st.SMTPHost, "://")) {
		return errSettings("smtp_host must be a hostname (no scheme, no spaces)")
	}
	if st.SMTPHost != "" {
		if st.SMTPFrom == "" || st.SMTPTo == "" {
			return errSettings("smtp_from and smtp_to are required when smtp_host is set")
		}
	} else if st.SMTPFrom != "" || st.SMTPTo != "" {
		return errSettings("smtp_host is required when smtp_from/smtp_to is set")
	}
	if st.SMTPFrom != "" {
		if _, err := mail.ParseAddress(st.SMTPFrom); err != nil {
			return errSettings("smtp_from must be a valid email address")
		}
	}
	if st.SMTPTo != "" {
		for _, addr := range strings.Split(st.SMTPTo, ",") {
			addr = strings.TrimSpace(addr)
			if addr == "" {
				return errSettings("smtp_to contains an empty recipient")
			}
			if _, err := mail.ParseAddress(addr); err != nil {
				return errSettings("smtp_to must be comma-separated valid email addresses")
			}
		}
	}
	// Controller URL (what nodes dial): required for provisioning, must be
	// an absolute http(s) URL. Empty is tolerated until someone provisions.
	st.ControllerURL = strings.TrimSpace(st.ControllerURL)
	if cu := st.ControllerURL; cu != "" {
		u, err := url.Parse(cu)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errSettings("controller_url must be a full http(s) URL (e.g. http://172.24.92.232:8080)")
		}
	}
	// Node paths must be absolute Windows paths (drive letter or UNC);
	// controller paths must be absolute Unix paths. A swapped mapping is the
	// classic mistake here.
	if !isWindowsPath(st.NodeBinDir) || !isWindowsPath(st.NodeScriptsDir) || !isWindowsPath(st.NodeReleaseDir) {
		return errSettings("node_*_dir paths must be absolute Windows paths (e.g. C:\\bin or \\\\server\\share)")
	}
	if !isUnixPath(st.ScriptsRoot) || !isUnixPath(st.ReleaseRoot) {
		return errSettings("scripts_root/release_root must be absolute Unix paths (e.g. /data/scripts)")
	}
	return nil
}

type settingsErr string

func (e settingsErr) Error() string { return string(e) }
func errSettings(msg string) error  { return settingsErr(msg) }

// isHTTPURL reports whether raw is an absolute http(s) URL with a host —
// the shared bound for the generic webhook and ntfy topic settings.
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// isWindowsPath accepts C:\... and C:/... (separator after the drive
// letter is REQUIRED — "C:bin" is drive-RELATIVE and must be rejected) plus
// \\server\share UNC forms.
func isWindowsPath(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	if len(p) >= 2 && p[:2] == `\\` {
		return true
	}
	return false
}

// isUnixPath accepts absolute container paths. The double-slash prefix is
// rejected because it is how Windows UNC paths often sneak in — the
// swapped-mapping guard is the whole point of this validation.
func isUnixPath(p string) bool {
	return len(p) > 0 && p[0] == '/' && !strings.HasPrefix(p, "//")
}
