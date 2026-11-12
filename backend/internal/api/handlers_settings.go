package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/offsite"
	"golang.org/x/crypto/ssh"
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
	// BackupOffsite shadows the nested offsite block to capture its
	// write-only secrets (the model's json:"-" drops them). nil = the PUT
	// omitted the block entirely → the stored block is kept as-is.
	BackupOffsite *offsiteReq `json:"backup_offsite,omitempty"`
	// Ingest shadows the nested ingestion block (P11) for its write-only
	// password. nil = the PUT omitted the block → the stored block is
	// kept as-is (same keep-semantics as backup_offsite).
	Ingest *ingestReq `json:"ingest,omitempty"`
}

// ingestReq mirrors the FULL ingest block for decoding (the shadow is
// the sole decode target under DisallowUnknownFields): model fields plus
// the write-only password. has_password is a GET-only echo, accepted and
// ignored so a verbatim GET→PUT round-trip keeps working.
type ingestReq struct {
	model.IngestSettings
	Password    string `json:"password"`
	HasPassword bool   `json:"has_password"` // output-only echo, ignored
}

// offsiteReq mirrors the FULL backup_offsite block for decoding: the
// handler decodes with DisallowUnknownFields, and this shadow (shallower
// than the embedded model.Settings field) is the sole decode target for
// "backup_offsite" — so it must carry every non-secret field too (via the
// embedded model types) plus the write-only secrets the model hides. The
// has_* flags are GET-only echoes (block-level, matching offsiteOut),
// accepted and ignored so a verbatim GET→PUT round-trip keeps working.
type offsiteReq struct {
	model.BackupOffsite
	S3                offsiteS3Req   `json:"s3"`
	SFTP              offsiteSFTPReq `json:"sftp"`
	HasS3SecretKey    bool           `json:"has_s3_secret_key"`    // output-only echo, ignored
	HasSFTPPassword   bool           `json:"has_sftp_password"`    // output-only echo, ignored
	HasSFTPPrivateKey bool           `json:"has_sftp_private_key"` // output-only echo, ignored
	HasS3AccessKey    bool           `json:"has_s3_access_key"`    // output-only echo, ignored
}

// offsiteS3Req is the S3 sub-block: model fields + the write-only secret.
type offsiteS3Req struct {
	model.BackupOffsiteS3
	SecretKey string `json:"secret_key"`
}

// offsiteSFTPReq is the SFTP sub-block: model fields + write-only secrets.
type offsiteSFTPReq struct {
	model.BackupOffsiteSFTP
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
}

// settingsOut is the API-facing settings: identical to model.Settings plus
// has_* flags for the write-only secrets (plaintext never leaves the
// controller — same mask pattern as shareOut).
type settingsOut struct {
	*model.Settings
	HasNtfyToken    bool `json:"has_ntfy_token"`
	HasSMTPPassword bool `json:"has_smtp_password"`
	// hasS3AccessKey overrides the presence flag derived from the (possibly
	// masked) settings: non-admin GETs empty the access_key value but must
	// still report that one is configured (Q2).
	hasS3AccessKey *bool
}

// offsiteOut is the API-facing backup_offsite block: the model fields plus
// has_* flags for the three write-only destination secrets. has_s3_access_key
// reports access_key presence for NON-admin readers, whose access_key value
// is masked (Q2) — admins see the value itself.
type offsiteOut struct {
	model.BackupOffsite
	HasS3SecretKey    bool `json:"has_s3_secret_key"`
	HasSFTPPassword   bool `json:"has_sftp_password"`
	HasSFTPPrivateKey bool `json:"has_sftp_private_key"`
	HasS3AccessKey    bool `json:"has_s3_access_key"`
}

// ingestOut is the API-facing ingest block: model fields (password is
// json:"-" on the model) plus the has_password presence flag.
type ingestOut struct {
	model.IngestSettings
	HasPassword bool `json:"has_password"`
}

// MarshalJSON implements the mask: embed Settings' fields (secrets already
// json:"-" on the model) and add the has_* flags. backup_offsite is
// re-marshaled through offsiteOut so its nested secrets get flags too.
func (o settingsOut) MarshalJSON() ([]byte, error) {
	type alias model.Settings
	off := o.Settings.BackupOffsite
	hasAccessKey := off.S3.AccessKey != ""
	if o.hasS3AccessKey != nil {
		hasAccessKey = *o.hasS3AccessKey
	}
	return json.Marshal(struct {
		alias
		HasNtfyToken    bool       `json:"has_ntfy_token"`
		HasSMTPPassword bool       `json:"has_smtp_password"`
		BackupOffsite   offsiteOut `json:"backup_offsite"`
		Ingest          ingestOut  `json:"ingest"`
	}{alias(*o.Settings), o.HasNtfyToken, o.HasSMTPPassword, offsiteOut{
		BackupOffsite:     off,
		HasS3SecretKey:    off.S3.SecretKey != "",
		HasSFTPPassword:   off.SFTP.Password != "",
		HasSFTPPrivateKey: off.SFTP.PrivateKey != "",
		HasS3AccessKey:    hasAccessKey,
	}, ingestOut{
		IngestSettings: o.Settings.Ingest,
		HasPassword:    o.Settings.Ingest.Password != "",
	}})
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
		// with env defaults instead of the operator's configured paths. The
		// read path stays available (fallback), but the failure is an ERROR:
		// the usual cause is a lost shares key making stored secrets
		// undecryptable, and the save path refuses to write until it is fixed.
		s.Log.Error("settings load failed, falling back to env defaults — stored settings unreadable (restore the shares key if it was lost)", "err", err)
	}
	if st != nil {
		return st
	}
	return s.defaultsSettings()
}

// handleGetSettings exposes the live settings (NFS share config, controller
// roots, remote path mapping, behavior) to the UI. Write-only secrets are
// masked to has_* flags. For NON-admin readers (viewer sessions, read-scope
// API tokens) the channel URLs' query VALUES are masked too — ntfy topic and
// generic webhook URLs can carry credentials (?auth=<token>), and this route
// is reachable by any authenticated user. The Discord webhook's PATH token
// and the offsite S3 access_key are masked as well (has_s3_access_key reports
// presence). Admins see everything.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	st := s.currentSettings(r.Context())
	out := maskSettings(st)
	if !requestIsAdmin(r) {
		// Presence must be computed BEFORE the mask empties the value.
		hasAccessKey := st.BackupOffsite.S3.AccessKey != ""
		st = maskSettingsForNonAdmin(st)
		out = maskSettings(st)
		out.hasS3AccessKey = &hasAccessKey
	}
	writeJSON(w, http.StatusOK, out)
}

// maskSettingsForNonAdmin returns a COPY of st with credential material
// masked for non-admin readers:
//   - query values in the two channel URLs (scheme/host/path stay visible
//     so the endpoint remains identifiable — the least-breaking mask),
//   - the Discord webhook's path token (RedactURL masks the last
//     /api/webhooks/ segment — the token IS the credential),
//   - the offsite S3 access_key (emptied; has_s3_access_key reports presence
//     — P4 review Q2: it is half of a credential pair and was fully visible).
//
// The only non-admin consumer is the Settings page's read-only fields, so
// masking breaks no workflow. The stored settings are never mutated.
// Dedicated secret fields are already write-only on the model (json:"-")
// for every role.
func maskSettingsForNonAdmin(st *model.Settings) *model.Settings {
	cp := *st
	cp.NtfyTopicURL = notify.RedactURL(cp.NtfyTopicURL)
	cp.GenericWebhookURL = notify.RedactURL(cp.GenericWebhookURL)
	// Discord's secret is a PATH token, not a query value — RedactURL's
	// discord.com/discordapp.com branch masks it (W9: this was the one
	// channel URL the non-admin mask missed).
	cp.DiscordWebhook = notify.RedactURL(cp.DiscordWebhook)
	cp.BackupOffsite.S3.AccessKey = ""
	// The ingestion client URL can carry query credentials the same way
	// the channel URLs can; mask its query values for non-admin readers.
	cp.Ingest.URL = notify.RedactURL(cp.Ingest.URL)
	return &cp
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
	// Offsite secrets arrive in the nested shadow (the model's json:"-"
	// drops them). An absent block keeps the stored one wholesale — see
	// the keep-merge below.
	if req.BackupOffsite != nil {
		st.BackupOffsite = req.BackupOffsite.BackupOffsite
		st.BackupOffsite.S3 = req.BackupOffsite.S3.BackupOffsiteS3
		st.BackupOffsite.SFTP = req.BackupOffsite.SFTP.BackupOffsiteSFTP
		st.BackupOffsite.S3.SecretKey = req.BackupOffsite.S3.SecretKey
		st.BackupOffsite.SFTP.Password = req.BackupOffsite.SFTP.Password
		st.BackupOffsite.SFTP.PrivateKey = req.BackupOffsite.SFTP.PrivateKey
	}
	// Ingest secret arrives in the nested shadow (the model's json:"-"
	// drops it). An absent block keeps the stored one wholesale — see
	// the keep-merge below.
	if req.Ingest != nil {
		st.Ingest = req.Ingest.IngestSettings
		st.Ingest.Password = req.Ingest.Password
	}
	if err := validateSettings(&st); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Keep-semantics for write-only secrets: blank = keep the stored value.
	// Read the stored row DIRECTLY — never via currentSettings(), whose
	// env-defaults fallback would merge from an UNKNOWN state: when the
	// stored ciphertext is undecryptable (shares key lost), merging from
	// the fallback silently wipes the stored secrets on the next unrelated
	// PUT. Only a genuinely absent row (nil, nil) may merge from defaults;
	// any read/decrypt error fails the save closed.
	prev, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.Log.Error("refusing settings save: stored settings unreadable", "err", err)
		writeErr(w, http.StatusInternalServerError,
			"stored settings unreadable — restore the shares key before saving (secrets were NOT changed)")
		return
	}
	if prev == nil {
		prev = s.defaultsSettings()
	}
	if st.NtfyToken == "" {
		st.NtfyToken = prev.NtfyToken
	}
	if st.SMTPPassword == "" {
		st.SMTPPassword = prev.SMTPPassword
	}
	// Offsite keep-semantics: an OMITTED backup_offsite block keeps the
	// stored block wholesale (a partial PUT from automation that predates
	// P4 must not wipe destination config). A PRESENT block replaces the
	// non-secret fields; each blank secret keeps its stored value.
	if req.BackupOffsite == nil {
		st.BackupOffsite = prev.BackupOffsite
	} else {
		if st.BackupOffsite.S3.SecretKey == "" {
			st.BackupOffsite.S3.SecretKey = prev.BackupOffsite.S3.SecretKey
		}
		if st.BackupOffsite.SFTP.Password == "" {
			st.BackupOffsite.SFTP.Password = prev.BackupOffsite.SFTP.Password
		}
		if st.BackupOffsite.SFTP.PrivateKey == "" {
			st.BackupOffsite.SFTP.PrivateKey = prev.BackupOffsite.SFTP.PrivateKey
		}
	}
	// Ingest keep-semantics (P11): an OMITTED ingest block keeps the
	// stored block wholesale (a partial PUT from automation that predates
	// P11 must not wipe ingestion config). A PRESENT block replaces the
	// non-secret fields; a blank password keeps its stored value.
	if req.Ingest == nil {
		st.Ingest = prev.Ingest
	} else if st.Ingest.Password == "" {
		st.Ingest.Password = prev.Ingest.Password
	}
	// G4: a configured SMTP user with no password AFTER the keep-merge
	// would silently send unauthenticated — refuse it. (Checked post-merge
	// so a stored password keeps the configuration valid when the PUT
	// omits the password.)
	if st.SMTPUser != "" && st.SMTPPassword == "" {
		writeErr(w, http.StatusBadRequest,
			"smtp_password is required when smtp_user is set (a blank password keeps the stored one; none is stored)")
		return
	}
	// Same post-merge rule for the offsite destinations: an enabled
	// destination without its credential (none stored, none supplied)
	// would fail every upload — refuse at save time instead.
	if st.BackupOffsite.S3.Enabled && st.BackupOffsite.S3.SecretKey == "" {
		writeErr(w, http.StatusBadRequest,
			"backup_offsite.s3.secret_key is required when the S3 destination is enabled (a blank value keeps the stored one; none is stored)")
		return
	}
	if st.BackupOffsite.SFTP.Enabled && st.BackupOffsite.SFTP.Password == "" && st.BackupOffsite.SFTP.PrivateKey == "" {
		writeErr(w, http.StatusBadRequest,
			"backup_offsite.sftp needs a password or private_key when enabled (blank values keep the stored ones; none are stored)")
		return
	}
	// Same post-merge rule for ingestion: a configured username with no
	// password AFTER the keep-merge would fail every client login —
	// refuse at save time. (No username at all is legal: qBittorrent with
	// localhost auth bypass, or ruTorrent without web-server auth.)
	if st.Ingest.Enabled && st.Ingest.Username != "" && st.Ingest.Password == "" {
		writeErr(w, http.StatusBadRequest,
			"ingest.password is required when ingest.username is set (a blank password keeps the stored one; none is stored)")
		return
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
		// Offsite: operational knobs + booleans only — endpoints, keys and
		// passwords never land in the audit table.
		"offsite_s3_enabled":         st.BackupOffsite.S3.Enabled,
		"offsite_sftp_enabled":       st.BackupOffsite.SFTP.Enabled,
		"offsite_keep_n":             st.BackupOffsite.KeepN,
		"offsite_include_shares_key": st.BackupOffsite.IncludeSharesKey,
		"offsite_s3_secret_changed":  req.BackupOffsite != nil && req.BackupOffsite.S3.SecretKey != "",
		"offsite_sftp_pw_changed":    req.BackupOffsite != nil && req.BackupOffsite.SFTP.Password != "",
		"offsite_sftp_key_changed":   req.BackupOffsite != nil && req.BackupOffsite.SFTP.PrivateKey != "",
		// Ingestion (P11): operational knobs + booleans only — the client
		// URL, username and password never land in the audit table.
		"ingest_enabled":          st.Ingest.Enabled,
		"ingest_provider":         st.Ingest.Provider,
		"ingest_password_changed": req.Ingest != nil && req.Ingest.Password != "",
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
		return errSettings("generic_webhook_url must be an http(s) URL without embedded credentials (user:pass@) or blank")
	}
	st.NtfyTopicURL = strings.TrimSpace(st.NtfyTopicURL)
	if st.NtfyTopicURL != "" && !isHTTPURL(st.NtfyTopicURL) {
		return errSettings("ntfy_topic_url must be an http(s) URL without embedded credentials (user:pass@) (e.g. https://ntfy.sh/mytopic) or blank")
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
	// A ':' in the host is only legal for a bracketed IPv6 literal — a
	// bare "host:25" would produce a malformed net.JoinHostPort at send
	// time (the port belongs in smtp_port). Brackets themselves are only
	// valid around an IPv6 literal, so "[hostname]" is refused too.
	if h := st.SMTPHost; strings.Contains(h, ":") || strings.HasPrefix(h, "[") || strings.HasSuffix(h, "]") {
		if !strings.HasPrefix(h, "[") || !strings.HasSuffix(h, "]") || net.ParseIP(h[1:len(h)-1]) == nil {
			return errSettings("smtp_host must be a hostname or a bracketed IPv6 literal like [::1] (set the port in smtp_port)")
		}
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
	if err := validateBackupOffsite(&st.BackupOffsite); err != nil {
		return err
	}
	if err := validateIngest(&st.Ingest); err != nil {
		return err
	}
	return nil
}

// validateBackupOffsite checks the offsite destination block (P4). Both
// destinations are optional; an ENABLED one must carry its connection
// fields. Credential presence is checked post-merge in the handler (a
// blank secret keeps the stored one). Normalization (endpoint form, prefix
// slashes) happens here so the stored value is always upload-ready.
func validateBackupOffsite(o *model.BackupOffsite) error {
	if o.KeepN < 0 || o.KeepN > 365 {
		return errSettings("backup_offsite.keep_n must be 0 (default 30) or 1-365")
	}
	s3 := &o.S3
	s3.Endpoint = strings.TrimSpace(s3.Endpoint)
	s3.Bucket = strings.TrimSpace(s3.Bucket)
	s3.Prefix = strings.TrimSpace(s3.Prefix)
	s3.AccessKey = strings.TrimSpace(s3.AccessKey)
	if s3.Enabled {
		if s3.Endpoint == "" {
			return errSettings("backup_offsite.s3.endpoint is required when the S3 destination is enabled")
		}
		ep, err := normalizeS3Endpoint(s3.Endpoint)
		if err != nil {
			return errSettings("backup_offsite.s3.endpoint: " + err.Error())
		}
		s3.Endpoint = ep
		if s3.Bucket == "" {
			return errSettings("backup_offsite.s3.bucket is required when the S3 destination is enabled")
		}
		if strings.ContainsAny(s3.Bucket, "/\\ 	") {
			return errSettings("backup_offsite.s3.bucket must be a plain bucket name (no slashes or spaces)")
		}
		if s3.AccessKey == "" {
			return errSettings("backup_offsite.s3.access_key is required when the S3 destination is enabled")
		}
	}
	// Prefix: optional key namespace. Normalize to no leading slash and a
	// trailing slash so uploads land at <prefix><name>; reject traversal.
	if s3.Prefix != "" {
		p := strings.Trim(s3.Prefix, "/")
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." || seg == "." {
				return errSettings("backup_offsite.s3.prefix must not contain . or .. segments")
			}
		}
		if p != "" {
			s3.Prefix = p + "/"
		} else {
			s3.Prefix = ""
		}
	}
	sf := &o.SFTP
	sf.Host = strings.TrimSpace(sf.Host)
	sf.User = strings.TrimSpace(sf.User)
	sf.Path = strings.TrimSpace(sf.Path)
	if sf.Port < 0 || sf.Port > 65535 {
		return errSettings("backup_offsite.sftp.port must be 0 (default 22) or 1-65535")
	}
	if sf.Enabled {
		if sf.Host == "" {
			return errSettings("backup_offsite.sftp.host is required when the SFTP destination is enabled")
		}
		if err := validateOffsiteHost("backup_offsite.sftp.host", sf.Host); err != nil {
			return err
		}
		if sf.User == "" {
			return errSettings("backup_offsite.sftp.user is required when the SFTP destination is enabled")
		}
		// D1: the bundle carries the shares key — an unpinned SFTP
		// connection is a MITM key-theft vector. Require the host key.
		if sf.HostKey == "" {
			return errSettings("backup_offsite.sftp.host_key is required when the SFTP destination is enabled — pin the server's host key (get it with: ssh-keyscan <host>)")
		}
	}
	// A supplied host key must parse NOW (catch paste errors at save time,
	// not mid-backup) and is stored normalized to "<type> <base64>".
	if sf.HostKey != "" {
		key, err := offsite.ParseHostKey(sf.HostKey)
		if err != nil {
			return errSettings("backup_offsite.sftp.host_key: " + err.Error())
		}
		sf.HostKey = offsite.NormalizeHostKey(key)
	}
	if sf.Path != "" {
		for _, seg := range strings.Split(sf.Path, "/") {
			if seg == ".." {
				return errSettings("backup_offsite.sftp.path must not contain .. segments")
			}
		}
	}
	// A supplied private key must parse NOW (catch paste errors at save
	// time, not mid-backup). Passphrase-protected keys are unsupported —
	// there is no passphrase field in the settings surface.
	if sf.PrivateKey != "" {
		if _, err := ssh.ParseRawPrivateKey([]byte(sf.PrivateKey)); err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				return errSettings("backup_offsite.sftp.private_key must be unencrypted (passphrase-protected keys are not supported)")
			}
			return errSettings("backup_offsite.sftp.private_key must be a valid SSH private key (PEM)")
		}
	}
	return nil
}

// validateOffsiteHost applies the P3 hostname rules (same as smtp_host):
// no scheme, no spaces, and a ':' only inside a bracketed IPv6 literal —
// the port belongs in the dedicated port field.
func validateOffsiteHost(field, h string) error {
	if strings.ContainsAny(h, " 	") || strings.Contains(h, "://") {
		return errSettings(field + " must be a hostname (no scheme, no spaces)")
	}
	if strings.Contains(h, ":") || strings.HasPrefix(h, "[") || strings.HasSuffix(h, "]") {
		if !strings.HasPrefix(h, "[") || !strings.HasSuffix(h, "]") || net.ParseIP(h[1:len(h)-1]) == nil {
			return errSettings(field + " must be a hostname or a bracketed IPv6 literal like [::1] (set the port in the port field)")
		}
	}
	return nil
}

// normalizeS3Endpoint accepts host, host:port or a full http(s) URL and
// returns the host[:port] form minio-go wants (no scheme). Unlike the SMTP
// host rule, a colon IS legal here as the port separator — that is the
// native S3 endpoint form; a bare colon without a valid port is rejected.
func normalizeS3Endpoint(raw string) (string, error) {
	ep := raw
	if strings.Contains(ep, "://") {
		u, err := url.Parse(ep)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", errors.New("must be host[:port] or an http(s) URL")
		}
		if u.Path != "" && u.Path != "/" {
			return "", errors.New("must not include a path")
		}
		ep = u.Host
	}
	if strings.ContainsAny(ep, " 	") {
		return "", errors.New("must not contain spaces")
	}
	if strings.HasPrefix(ep, "[") {
		// Bracketed IPv6, with or without a port.
		host, port, err := net.SplitHostPort(ep)
		if err != nil {
			if !strings.HasSuffix(ep, "]") || net.ParseIP(ep[1:len(ep)-1]) == nil {
				return "", errors.New("invalid bracketed IPv6 literal")
			}
			return ep, nil
		}
		if net.ParseIP(host) == nil {
			return "", errors.New("invalid IPv6 literal")
		}
		if err := validatePort(port); err != nil {
			return "", err
		}
		return ep, nil
	}
	if strings.Contains(ep, ":") {
		host, port, err := net.SplitHostPort(ep)
		if err != nil || host == "" {
			return "", errors.New("invalid host:port")
		}
		if err := validatePort(port); err != nil {
			return "", err
		}
		return net.JoinHostPort(host, port), nil
	}
	return ep, nil
}

// validatePort checks a numeric port string is 1-65535.
func validatePort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be 1-65535")
	}
	return nil
}

type settingsErr string

func (e settingsErr) Error() string { return string(e) }
func errSettings(msg string) error  { return settingsErr(msg) }

// isHTTPURL reports whether raw is an absolute http(s) URL with a host
// and NO embedded userinfo — the shared bound for the generic webhook,
// ntfy topic and ingestion client settings. Credentials belong in the
// dedicated username/password fields, never in the URL: transport
// errors quote the full URL and would leak them into logs (Q4).
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && (u.Scheme == "http" || u.Scheme == "https")
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
