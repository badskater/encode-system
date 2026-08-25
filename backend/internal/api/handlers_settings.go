package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Settings (NFS shares, roots, remote path mapping) ----------

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
// roots, remote path mapping, behavior) to the UI.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentSettings(r.Context()))
}

// handleUpdateSettings replaces the settings row. Validation keeps the
// operator from shooting themselves in the foot: paths must be absolute on
// their respective OS, intervals sane, required fields present.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req model.Settings
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateSettings(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.SaveSettings(r.Context(), &req); err != nil {
		writeErr(w, http.StatusInternalServerError, "save settings")
		return
	}
	s.Log.Info("settings updated via UI", "scripts_root", req.ScriptsRoot,
		"node_bin_dir", req.NodeBinDir, "scan_interval_s", req.ScanIntervalSeconds)
	writeJSON(w, http.StatusOK, s.currentSettings(r.Context()))
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
