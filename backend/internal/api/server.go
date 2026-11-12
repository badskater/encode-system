// Package api implements the controller's HTTP API: agent endpoints
// (heartbeat, job lifecycle, updates) and UI endpoints (nodes, jobs, flows).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/offsite"
	"github.com/badskater/encode-system/backend/internal/oidc"
	"github.com/badskater/encode-system/backend/internal/provision"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
	"golang.org/x/crypto/bcrypt"
)

// Config carries runtime settings injected into handlers.
type Config struct {
	AdminUsername       string        // management-plane admin account (seeded at startup)
	AdminPassword       string        // initial password for the admin account (only used when the account doesn't exist)
	ForceAdminPassword  bool          // recovery hatch: overwrite the stored admin hash with AdminPassword on this boot
	ScriptsRoot         string        // controller-side scripts share mount
	ReleaseRoot         string        // controller-side release share mount
	NodeBinDir          string        // tools dir on nodes, e.g. C:\bin
	NodeScriptsDir      string        // scripts mount on nodes, e.g. C:\Encodes\scripts
	NodeReleaseDir      string        // release mount on nodes
	Group               string        // release group tag
	Tag                 string        // quality tag, e.g. 1080p
	TasksBeforeReboot   int           // reboot threshold (default 10)
	ScanIntervalSeconds int           // scanner cadence (default 30)
	RebootGracePeriod   time.Duration // reboot attempt expires after this (default 10m)
	StaleAfter          time.Duration // node offline after no heartbeat this long
	DefaultFlowName     string
	DiscordWebhook      string
	// InstanceID identifies THIS controller replica for HA: it is stamped
	// into the slog logger (default attrs) and exposed in /api/health as the
	// "instance" field so an operator can tell two replicas apart. Resolved
	// in main from ENCODE_INSTANCE_ID (defaulting to os.Hostname); empty
	// means the default was not set (New fills it with "controller" so the
	// field is never blank in logs/health). Purely additive — the existing
	// {"status":"ok"} health field stays.
	InstanceID string
	// GitHubToken is an optional PAT for authenticated GitHub API calls
	// (update check). Empty = unauthenticated requests (lower rate limit).
	GitHubToken string
	// GitHubRepo is the "owner/repo" to check for latest releases.
	// Defaults to "badskater/encode-system".
	GitHubRepo string
	// GitHubAPI is the GitHub API base URL (default https://api.github.com).
	// Overridable for tests or GitHub Enterprise (https://ghe.example/api/v3).
	GitHubAPI string
	// Version is the controller build version (set via ldflags -X main.Version
	// in CI). Empty in dev builds; the update check reports it as "unknown".
	Version string
	// OIDC carries the validated optional OIDC login configuration
	// (ENCODE_OIDC_* env vars, resolved in main). nil = disabled (the
	// default): no OIDC routes are registered and /api/health reports
	// oidc_enabled=false. Local login is unaffected either way.
	OIDC *oidc.Config
}

// Server bundles dependencies for all handlers.
type Server struct {
	Store     *store.Store
	Update    *update.Store
	Log       *slog.Logger
	Cfg       Config
	Provision *provision.Engine // node provisioning (nil = unavailable)
	// Backup owns DB snapshot scheduling (scheduled + manual). Set by main
	// after New; nil in tests that don't exercise backup routes (handlers
	// 503 when nil).
	Backup   *BackupManager
	throttle *loginThrottle
	// oidcThrottle is the login-start lockout for /api/auth/oidc/login —
	// the same mechanism as throttle but a SEPARATE counter, so OIDC bursts
	// can never lock out local login (and vice versa).
	oidcThrottle *loginThrottle
	// Notifier, when non-nil, overrides the live Discord notifier resolved
	// from settings on each job-outcome alert. Tests inject a recording
	// notifier here to assert whether a notification fired (the auto-retry
	// path must be silent). Production leaves this nil so notifyJobFinished
	// resolves the webhook from currentSettings exactly as before.
	Notifier notify.Notifier
	// MailSender, when non-nil, overrides the net/smtp send seam used by the
	// email notification channel. Tests inject a fake to assert envelope +
	// content without a real relay. Production leaves this nil (real
	// net/smtp.SendMail).
	MailSender notify.SMTPSender
	// OffsiteDestinations, when non-nil, overrides offsite destination
	// construction (P4). Tests inject fakes to assert upload/retention
	// behavior without real S3/SSH endpoints; production leaves this nil
	// (offsite.BuildDestinations from live settings). Same seam pattern as
	// Notifier/MailSender.
	OffsiteDestinations func(st *model.Settings) ([]offsite.Destination, []offsite.Result)
	// digest holds buffered job-outcome events when settings.NotifyDigest is
	// ON. A ticker started by StartDigestLoop flushes it hourly, posting one
	// summary instead of per-job alerts. Built in New (never nil — the buffer
	// is always present so notifyJobFinished can push without a nil check on
	// the state; only the loop is started later). In-memory only — a restart
	// loses pending events (acceptable: observability data, not state; the
	// jobs table is the source of truth).
	digest *digestState
	// logHub fans out live job-progress events to SSE subscribers of
	// GET /api/jobs/{id}/log/stream. Built in New (never nil) so the
	// heartbeat/completion paths publish unconditionally; in-memory only —
	// subscribers reconnect and re-snapshot after a controller restart.
	logHub *logHub
	// diskGuard tracks per-node disk-alert cooldowns so a node hovering at
	// the threshold alerts once per window instead of every heartbeat.
	// Built in New (never nil). In-memory only — a restart resets cooldowns
	// (acceptable: worst case one extra alert after a restart).
	diskGuard *diskGuardState
	// scanTrigger is a buffered-1 channel fed by POST /api/scan to request
	// an immediate scan out-of-cycle. The scanner loop's select listens on
	// it alongside its ticker. Buffered-1 so a non-blocking send always
	// succeeds (a second request while one is pending coalesces — the loop
	// drains one signal and the next scan covers both). On HA (Postgres)
	// only the leader replica's scanner holds the advisory lock, so a
	// trigger on a non-leader is a no-op (the channel is never consumed).
	scanTrigger chan struct{}
	// nodeAlert holds the in-memory baseline of node online/offline states
	// for the node-status alert loop (StartNodeAlertLoop). Built in New
	// (never nil) so nodeAlertTick can use it without a nil check. The
	// baseline is seeded silently on the first tick after a restart.
	nodeAlert *nodeAlertState
	// pendingCancels queues per-node job IDs for the "cancel" heartbeat
	// instruction (F5): the stall watchdog fails a job server-side, but an
	// online node keeps the wedged process — the cancel tells the agent to
	// kill the tree and free the slot. Guarded by cancelMu; drained into
	// the node's next heartbeat reply, cleared on offline/deletion/re-pair
	// so unreachable nodes never accumulate state. In-memory only: a
	// controller restart loses queued cancels (acceptable — the job is
	// already failed in the DB, and orphan recovery re-evaluates on the
	// node's next heartbeat).
	cancelMu       sync.Mutex
	pendingCancels map[int64][]int64
	// OIDC drives the optional SSO login flow (P10). Built in New when
	// Config.OIDC is non-nil; nil = disabled, and Routes() then registers
	// NO oidc endpoints (404) — zero behavior change for local-only installs.
	OIDC *oidc.Provider
}

// digestState bundles the buffer + flush plumbing so Server carries one
// field instead of two. Built by New (the buffer is always present so
// notifyJobFinished can push without a nil check on the state itself);
// StartDigestLoop starts the ticker goroutine that flushes it hourly.
type digestState struct {
	buf *notify.DigestBuffer
}

// New builds the server and seeds the default flow when absent.
func New(st *store.Store, up *update.Store, log *slog.Logger, cfg Config) (*Server, error) {
	if cfg.TasksBeforeReboot <= 0 {
		cfg.TasksBeforeReboot = 10
	}
	if cfg.ScanIntervalSeconds <= 0 {
		cfg.ScanIntervalSeconds = 30
	}
	if cfg.RebootGracePeriod <= 0 {
		cfg.RebootGracePeriod = 10 * time.Minute
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 45 * time.Second
	}
	if cfg.DefaultFlowName == "" {
		cfg.DefaultFlowName = "default-1080"
	}
	// InstanceID: never blank in logs/health. main resolves it from
	// ENCODE_INSTANCE_ID (default os.Hostname); if somehow empty here, fall
	// back to "controller" so the field is always present and distinguishable
	// from a real two-replica run where both would share the same blank.
	if cfg.InstanceID == "" {
		cfg.InstanceID = "controller"
	}
	s := &Server{
		Store: st, Update: up, Log: log, Cfg: cfg, throttle: &loginThrottle{},
		oidcThrottle:   &loginThrottle{},
		digest:         &digestState{buf: notify.NewDigestBuffer()},
		logHub:         newLogHub(),
		diskGuard:      &diskGuardState{last: map[int64]time.Time{}},
		scanTrigger:    make(chan struct{}, 1),
		nodeAlert:      &nodeAlertState{prev: map[int64]bool{}},
		pendingCancels: map[int64][]int64{},
	}
	if cfg.DiscordWebhook != "" {
		log.Info("discord notifications enabled (default; override via Settings page)")
	}
	if cfg.OIDC != nil {
		// No network I/O here: discovery is lazy (first login attempt), so
		// the controller boots even while the IdP is unreachable.
		s.OIDC = oidc.NewProvider(cfg.OIDC, log)
		log.Info("OIDC login enabled", "issuer", cfg.OIDC.Issuer, "admin_group", cfg.OIDC.AdminGroup, "groups_claim", cfg.OIDC.GroupsClaim)
	}
	if err := s.seedStepTemplates(); err != nil {
		return nil, err
	}
	if err := s.seedDefaultFlow(); err != nil {
		return nil, err
	}
	if err := s.migrateDiscordWebhook(); err != nil {
		return nil, err
	}
	if err := s.seedAdminUser(); err != nil {
		return nil, err
	}
	return s, nil
}

// migrateDiscordWebhook repairs installs where settings were saved by a
// build that predates the discord_webhook key: such rows lack the key, and
// the saved-row-is-authoritative rule would otherwise silently disable the
// env-configured webhook on upgrade. The env value is injected exactly once
// (key present afterwards, operator free to blank it). Fresh installs and
// installs without a saved row need nothing — currentSettings falls back to
// the env defaults.
func (s *Server) migrateDiscordWebhook() error {
	if s.Cfg.DiscordWebhook == "" {
		return nil // nothing to inject; a missing key means "off" either way
	}
	ctx := ctxBg()
	migrated, err := s.Store.MigrateSettingsKey(ctx, "discord_webhook", s.Cfg.DiscordWebhook)
	if err != nil {
		return fmt.Errorf("migrate settings discord_webhook: %w", err)
	}
	if migrated {
		s.Log.Info("injected env Discord webhook into the saved settings row (key added by this release)")
	}
	return nil
}

// digestInterval is the flush cadence for the hourly digest loop. The loop
// rebuilds the ticker only if a future variant makes this configurable; for
// now it is a fixed hourly tick.
const digestInterval = time.Hour

// StartDigestLoop launches the hourly digest flusher as a background goroutine
// bound to ctx. It is modeled on scanner.RunLoop's lifecycle: ONE ticker is
// kept alive and stopped via defer at function exit so a panic in the flush
// path cannot leak it, and the interval is re-read each cycle so a future
// configurable cadence would apply on the next tick without a restart.
//
// Each tick reads the LIVE settings. When NotifyDigest is OFF the tick is a
// no-op (the buffer keeps any events buffered during a previous ON period;
// they flush on the next tick where digest is ON again — this is acceptable
// because the buffer is observability data, not state, and the jobs table
// remains the source of truth). When ON, the buffer is drained and, if
// non-empty, posted as a single Discord summary via FlushDigest. The webhook
// and controller URL are resolved live per flush so Settings-page edits apply
// immediately. A controller restart loses any buffered-but-unflushed events
// (acceptable — see DigestBuffer docs).
//
// Safe to call when s.digest is nil (a no-op guard lets tests skip the loop).
//
// HA: on Postgres the loop body runs under store.RunLoopSupervisor with
// LockKeyDigest, so exactly one replica flushes the hourly digest (a second
// replica's flush would double-post the same buffered events if the buffer
// were shared — it isn't, each replica has its own in-memory digest buffer,
// but the lock still prevents redundant flushes). On SQLite the supervisor
// is a transparent always-locked wrapper and the loop runs unchanged. The
// existing ctx-cancel/SIGTERM stop contract is preserved (the supervisor
// cancels the body's sub-ctx on parent cancel).
func (s *Server) StartDigestLoop(ctx context.Context) {
	if s.digest == nil || s.digest.buf == nil {
		return // no buffer initialized; nothing to flush (test/edge guard)
	}
	go store.RunLoopSupervisor(ctx, s.Store, s.Log, store.LockKeyDigest, "digest", func(ctx context.Context) {
		s.runDigestLoop(ctx)
	})
}

func (s *Server) runDigestLoop(ctx context.Context) {
	tick := time.NewTicker(digestInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.flushDigestTick(ctx)
		}
	}
}

// flushDigestTick runs one digest flush. Live settings drive whether the
// flush actually posts: digest OFF → the tick is silent (buffer retains
// events from a previous ON period for the next ON tick); digest ON → drain
// + fan the summary out to every configured channel.
func (s *Server) flushDigestTick(ctx context.Context) {
	st := s.currentSettings(ctx)
	if !st.NotifyDigest {
		return // digest off; buffered events wait for the next ON tick
	}
	notify.FlushDigestTo(s.Log, st, s.mailSender(), s.digest.buf, time.Now().UTC())
}

// mailSender returns the SMTP send seam: the injected fake when set (tests),
// else nil so the notify package selects the production net/smtp sender.
func (s *Server) mailSender() notify.SMTPSender {
	return s.MailSender
}

// seedAdminUser ensures the management-plane admin account exists. On first
// boot the password comes from Config (ENCODE_ADMIN_PASSWORD); on later boots
// an existing account is left untouched — passwords rotate through the API.
func (s *Server) seedAdminUser() error {
	username := s.Cfg.AdminUsername
	if username == "" {
		username = "admin"
	}
	existing, err := s.Store.UserByUsername(ctxBg(), username)
	if err != nil {
		return err
	}
	if existing != nil {
		// Recovery hatch only: with the explicit force flag set, overwrite
		// the stored hash so an operator who lost the password can get back
		// in, then rotate from the UI and remove the env again. Without the
		// flag an existing account is never touched by env values.
		if s.Cfg.ForceAdminPassword {
			if s.Cfg.AdminPassword == "" {
				s.Log.Error("ENCODE_ADMIN_FORCE_PASSWORD is set but ENCODE_ADMIN_PASSWORD is empty — recovery skipped", "username", username)
				return nil
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(s.Cfg.AdminPassword), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			if err := s.Store.UpdateUserPassword(ctxBg(), existing.ID, string(hash)); err != nil {
				return err
			}
			// A force-reset may follow a compromise, not just a lost
			// password: revoke EVERY session so no pre-reset token survives.
			if err := s.Store.DeleteUserSessions(ctxBg(), existing.ID, ""); err != nil {
				s.Log.Warn("revoke sessions after force-reset", "err", err)
			}
			s.Log.Error("admin password FORCE-RESET from environment (recovery hatch) — rotate it from the UI and UNSET both env vars; leaving the flag set re-applies this password on every restart", "username", username)
		}
		return nil // already provisioned
	}
	if s.Cfg.AdminPassword == "" {
		return fmt.Errorf("admin user %q does not exist and no password was configured", username)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(s.Cfg.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := s.Store.CreateUser(ctxBg(), username, string(hash), "admin"); err != nil {
		return err
	}
	s.Log.Info("management admin account created", "username", username)
	return nil
}

// seedStepTemplates installs the built-in pipeline sections on first boot.
// Existing templates are NEVER overwritten: UI edits to a built-in step
// persist across restarts. New built-ins added in a release appear on the
// next boot; a deliberate restore is available via
// POST /api/step-templates/{id}/reset.
func (s *Server) seedStepTemplates() error {
	ctx := ctxBg()
	for _, t := range flow.BuiltinStepTemplates() {
		if _, err := s.Store.InsertStepTemplateIfAbsent(ctx, t); err != nil {
			return fmt.Errorf("seed step template %s: %w", t.Key, err)
		}
	}
	// Guarded factory upgrade chain for the mux template: installs where a
	// pre-V3 mux template was already seeded keep it forever otherwise — only
	// untouched factory copies get upgraded. V1 (pre-FLAC) and V2 (FLAC-aware,
	// no language handling) both advance to the current language-aware factory
	// version. User-edited mux scripts never match the byte-for-byte guard and
	// stay in effect.
	if upgraded, err := s.Store.UpgradeStepTemplateIfFactory(ctx, "mux", flow.MuxFactoryV1, flow.MuxTemplate()); err != nil {
		return fmt.Errorf("upgrade mux template: %w", err)
	} else if upgraded {
		s.Log.Info("upgraded mux step template to the current factory version (from V1)")
	}
	if upgraded, err := s.Store.UpgradeStepTemplateIfFactory(ctx, "mux", flow.MuxFactoryV2, flow.MuxTemplate()); err != nil {
		return fmt.Errorf("upgrade mux template: %w", err)
	} else if upgraded {
		s.Log.Info("upgraded mux step template to the language-aware factory version (from V2)")
	}
	// Guarded encode_4k upgrade: V1 (HDR10/HLG signaling only) -> current
	// factory version (Dolby Vision RPU support + corrected color spellings).
	// Same byte-for-byte guard: user-edited encode_4k scripts stay in effect.
	if upgraded, err := s.Store.UpgradeStepTemplateIfFactory(ctx, "encode_4k", flow.Encode4kFactoryV1, flow.Encode4kTemplate()); err != nil {
		return fmt.Errorf("upgrade encode_4k template: %w", err)
	} else if upgraded {
		s.Log.Info("upgraded encode_4k step template to the Dolby Vision factory version")
	}
	// Guarded hdr_probe upgrade: V1 only matched /String-suffixed MediaInfo
	// field names; CLI MediaInfo (>=22.x) emits plain names (HDR_Format,
	// DolbyVision_Profile), so DV sources misclassified as plain HDR10.
	if upgraded, err := s.Store.UpgradeStepTemplateIfFactory(ctx, "hdr_probe", flow.HdrProbeFactoryV1, flow.HdrProbeTemplate()); err != nil {
		return fmt.Errorf("upgrade hdr_probe template: %w", err)
	} else if upgraded {
		s.Log.Info("upgraded hdr_probe step template to the CLI-MediaInfo-compatible factory version")
	}
	return nil
}

// seedDefaultFlow installs the standard flows on first boot and ensures
// exactly one flow carries the default flag. When no flow is default yet
// (fresh installs and databases created before the flag existed), the
// configured default flow takes the flag. Seeding is name-guarded: an
// existing flow (factory or user-edited) is never touched.
func (s *Server) seedDefaultFlow() error {
	ctx := ctxBg()
	for _, seed := range []*model.Flow{flow.DefaultFlow(), flow.Default4kFlow(), flow.Default4kCPUFlow(), flow.DefaultSubsFlow()} {
		if _, err := s.Store.FlowByName(ctx, seed.Name); err != nil {
			if _, err := s.Store.CreateFlow(ctx, seed); err != nil {
				// HA first-boot race: two replicas can both pass the name
				// check before either inserts. A UNIQUE violation means the
				// other replica already seeded this flow — re-check and
				// continue silently instead of failing startup.
				if isUniqueConstraintErr(err) {
					if _, recheckErr := s.Store.FlowByName(ctx, seed.Name); recheckErr == nil {
						continue
					}
				}
				return fmt.Errorf("seed flow %q: %w", seed.Name, err)
			}
			s.Log.Info("seeded flow", "name", seed.Name, "steps", len(seed.Steps))
		}
	}
	fl, err := s.Store.FlowByName(ctx, s.Cfg.DefaultFlowName)
	if err != nil {
		// Compat: installs may configure a custom default-flow name via
		// ENCODE_DEFAULT_FLOW; seed the 1080p factory flow under that name
		// exactly as older builds did.
		def := flow.DefaultFlow()
		def.Name = s.Cfg.DefaultFlowName
		if _, err := s.Store.CreateFlow(ctx, def); err != nil {
			// Same concurrent-seed tolerance as the factory loop above.
			if !isUniqueConstraintErr(err) {
				return fmt.Errorf("seed default flow %q: %w", def.Name, err)
			}
		} else {
			s.Log.Info("seeded flow", "name", def.Name, "steps", len(def.Steps))
		}
		if fl, err = s.Store.FlowByName(ctx, s.Cfg.DefaultFlowName); err != nil {
			return fmt.Errorf("resolve default flow: %w", err)
		}
	}
	// Only mark the flag when no flow carries it yet — an operator's
	// "make default" choice on the Flows page must survive restarts.
	if _, err := s.Store.DefaultFlow(ctx); err != nil {
		if err := s.Store.SetDefaultFlow(ctx, fl.ID); err != nil {
			return fmt.Errorf("mark default flow: %w", err)
		}
		s.Log.Info("marked default flow", "flow", fl.Name)
	}
	return nil
}

// Routes wires the mux for both agent and UI endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)

	// Agent endpoints — node token auth.
	mux.HandleFunc("POST /api/agent/heartbeat", s.withNodeAuth(s.handleHeartbeat))
	mux.HandleFunc("POST /api/agent/job/{id}/complete", s.withNodeAuth(s.handleJobComplete))
	mux.HandleFunc("GET /api/agent/manifest", s.withNodeAuth(s.handleManifest))
	mux.HandleFunc("GET /api/agent/download/agent", s.withNodeAuth(s.handleDownloadAgent))
	mux.HandleFunc("GET /api/agent/download/lib", s.withNodeAuth(s.handleDownloadLib))

	// Authentication — no session required to log in.
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.withAnyUser(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.withAnyUser(s.handleMe))
	mux.HandleFunc("POST /api/auth/password", s.withAdmin(s.handleChangePassword))

	// Optional OIDC login (P10): registered ONLY when enabled via env.
	// Disabled installs have no such routes (404) — local auth is the only
	// way in, byte-identical to before.
	if s.OIDC != nil {
		mux.HandleFunc("GET /api/auth/oidc/login", s.handleOIDCLogin)
		mux.HandleFunc("GET /api/auth/oidc/callback", s.handleOIDCCallback)
	}

	// UI endpoints — session auth. GET routes use withAnyUser so viewers
	// can read; all mutations (POST/PUT/PATCH/DELETE) stay withAdmin.
	// Management surfaces (/api/users, /api/tokens, /api/audit, /api/backup*)
	// remain admin-only even for GET.
	mux.HandleFunc("GET /api/nodes", s.withAnyUser(s.handleListNodes))
	mux.HandleFunc("POST /api/nodes", s.withAdmin(s.handleCreateNode))
	mux.HandleFunc("PATCH /api/nodes/{id}", s.withAdmin(s.handlePatchNode))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.withAdmin(s.handleDeleteNode))
	mux.HandleFunc("POST /api/nodes/{id}/reboot", s.withAdmin(s.handleRebootNode))
	mux.HandleFunc("GET /api/nodes/{id}/metrics", s.withAnyUser(s.handleNodeMetrics))

	mux.HandleFunc("GET /api/jobs", s.withAnyUser(s.handleListJobs))
	mux.HandleFunc("POST /api/jobs", s.withAdmin(s.handleCreateJob))
	mux.HandleFunc("POST /api/jobs/bulk", s.withAdmin(s.handleBulkJobs))
	mux.HandleFunc("POST /api/jobs/prune", s.withAdmin(s.handlePruneJobs))
	mux.HandleFunc("GET /api/jobs/{id}/eta", s.withAnyUser(s.handleJobETA))

	// Scan-now trigger: admin requests an immediate scan of the scripts
	// share out-of-cycle. Non-blocking: 202 even when already queued. On HA
	// only the leader's scanner consumes the channel; a non-leader trigger
	// is a no-op (the request still succeeds — the operator's intent is
	// "scan now" and the leader will do it on its own schedule regardless).
	mux.HandleFunc("POST /api/scan", s.withAdmin(s.handleScanNow))
	// Prometheus scrape endpoint: unauthenticated by design (aggregate
	// operational numbers only; scrapers authenticate at network layer).
	mux.HandleFunc("GET /metrics", s.handlePrometheus)
	mux.HandleFunc("GET /api/shares", s.withAnyUser(s.handleListShares))
	mux.HandleFunc("POST /api/shares", s.withAdmin(s.handleCreateShare))
	mux.HandleFunc("GET /api/shares/{id}", s.withAnyUser(s.handleGetShare))
	mux.HandleFunc("PUT /api/shares/{id}", s.withAdmin(s.handleUpdateShare))
	mux.HandleFunc("DELETE /api/shares/{id}", s.withAdmin(s.handleDeleteShare))
	mux.HandleFunc("GET /api/audit", s.withAdmin(s.handleListAudit))
	mux.HandleFunc("GET /api/tokens", s.withAdmin(s.handleListAPITokens))
	mux.HandleFunc("POST /api/tokens", s.withAdmin(s.handleCreateAPIToken))
	mux.HandleFunc("DELETE /api/tokens/{id}", s.withAdmin(s.handleDeleteAPIToken))
	mux.HandleFunc("GET /api/backup", s.withAdmin(s.handleBackupStatus))
	mux.HandleFunc("POST /api/backup", s.withAdmin(s.handleBackupNow))
	mux.HandleFunc("PUT /api/backup/settings", s.withAdmin(s.handleUpdateBackupSettings))
	mux.HandleFunc("GET /api/backup/{name}", s.withAdmin(s.handleBackupDownload))
	mux.HandleFunc("DELETE /api/backup/{name}", s.withAdmin(s.handleBackupDelete))
	mux.HandleFunc("POST /api/backup/{name}/restore", s.withAdmin(s.handleBackupRestore))
	mux.HandleFunc("GET /api/jobs/{id}", s.withAnyUser(s.handleGetJob))
	mux.HandleFunc("GET /api/jobs/{id}/log", s.withAnyUser(s.handleGetJobLog))
	mux.HandleFunc("GET /api/jobs/{id}/log/stream", s.withAnyUser(s.handleJobLogStream))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.withAdmin(s.handleRetryJob))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.withAdmin(s.handleCancelJob))
	mux.HandleFunc("PATCH /api/jobs/{id}", s.withAdmin(s.handlePatchJob))
	mux.HandleFunc("DELETE /api/jobs/{id}", s.withAdmin(s.handleDeleteJob))

	mux.HandleFunc("GET /api/flows", s.withAnyUser(s.handleListFlows))
	mux.HandleFunc("POST /api/flows", s.withAdmin(s.handleCreateFlow))
	mux.HandleFunc("PUT /api/flows/{id}", s.withAdmin(s.handleUpdateFlow))
	mux.HandleFunc("DELETE /api/flows/{id}", s.withAdmin(s.handleDeleteFlow))
	mux.HandleFunc("POST /api/flows/{id}/default", s.withAdmin(s.handleSetDefaultFlow))
	mux.HandleFunc("POST /api/flows/{id}/duplicate", s.withAdmin(s.handleDuplicateFlow))
	mux.HandleFunc("GET /api/flows/{id}/export", s.withAnyUser(s.handleExportFlow))
	mux.HandleFunc("POST /api/flows/import", s.withAdmin(s.handleImportFlow))

	mux.HandleFunc("GET /api/series", s.withAnyUser(s.handleListSeries))
	mux.HandleFunc("POST /api/series", s.withAdmin(s.handleCreateSeries))
	mux.HandleFunc("PATCH /api/series/{id}", s.withAdmin(s.handlePatchSeries))
	mux.HandleFunc("DELETE /api/series/{id}", s.withAdmin(s.handleDeleteSeries))

	mux.HandleFunc("GET /api/step-templates", s.withAnyUser(s.handleListStepTemplates))
	mux.HandleFunc("POST /api/step-templates", s.withAdmin(s.handleCreateStepTemplate))
	mux.HandleFunc("PUT /api/step-templates/{id}", s.withAdmin(s.handleUpdateStepTemplate))
	mux.HandleFunc("DELETE /api/step-templates/{id}", s.withAdmin(s.handleDeleteStepTemplate))
	mux.HandleFunc("POST /api/step-templates/{id}/reset", s.withAdmin(s.handleResetStepTemplate))

	mux.HandleFunc("GET /api/pairing", s.withAdmin(s.handleListPairingCodes))
	mux.HandleFunc("POST /api/pairing", s.withAdmin(s.handleCreatePairingCode))
	mux.HandleFunc("POST /api/agent/pair", s.handleAgentPair)

	// Node provisioning (controller-driven Ansible).
	mux.HandleFunc("POST /api/provision", s.withAdmin(s.handleStartProvision))
	mux.HandleFunc("GET /api/provision/runs", s.withAdmin(s.handleListProvisionRuns))
	mux.HandleFunc("GET /api/provision/runs/{id}", s.withAdmin(s.handleGetProvisionRunLog))

	mux.HandleFunc("GET /api/settings", s.withAnyUser(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.withAdmin(s.handleUpdateSettings))

	// Fleet stats — aggregate job-history snapshot (Phase E).
	mux.HandleFunc("GET /api/stats", s.withAnyUser(s.handleStats))

	// Update check: compare the running controller version to the latest
	// GitHub release. Viewer-readable (operators check for updates without
	// needing admin). The handler falls back to 502 on upstream errors.
	mux.HandleFunc("GET /api/updates/check", s.withAnyUser(s.handleUpdateCheck))

	// User management — admin-only (even GET /api/users is admin so viewers
	// can't enumerate accounts).
	mux.HandleFunc("GET /api/users", s.withAdmin(s.handleListUsers))
	mux.HandleFunc("POST /api/users", s.withAdmin(s.handleCreateUser))
	mux.HandleFunc("DELETE /api/users/{id}", s.withAdmin(s.handleDeleteUser))
	mux.HandleFunc("PUT /api/users/{id}/password", s.withAdmin(s.handleResetUserPassword))

	// Agent payloads: the update store serves agent binary, EncodeLib.ps1,
	// and the bin-folder zip package to nodes (auth: node token).
	mux.HandleFunc("GET /api/agent/download/bin", s.withNodeAuth(s.handleDownloadBin))

	// Publishing: upload new agent/lib/bin payloads from the UI.
	mux.HandleFunc("GET /api/updates/manifest", s.withAdmin(s.handleManifestAdmin))
	mux.HandleFunc("POST /api/updates/agent", s.withAdmin(s.handlePublishAgent))
	mux.HandleFunc("POST /api/updates/agent/rollback", s.withAdmin(s.handleRollbackAgent))
	mux.HandleFunc("POST /api/updates/lib", s.withAdmin(s.handlePublishLib))
	mux.HandleFunc("POST /api/updates/bin", s.withAdmin(s.handlePublishBin))
	mux.HandleFunc("POST /api/updates/bin/url", s.withAdmin(s.handlePublishBinFromURL))

	return logRequests(s.Log, mux)
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Flush forwards http.Flusher to the wrapped writer so streaming handlers
// (SSE log stream) survive the logging middleware. A wrapper without this
// hides the capability and w.(http.Flusher) fails at the handler.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests emits one structured log line per request with the status.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	return decodeJSONLimit(r, v, maxBodyBytes)
}

// decodeJSONLimit is like decodeJSON but with a caller-specified body cap. It
// is used by the job-complete route, which carries a full run.log (up to 1 MiB
// pre-JSON-escaping) plus step timings — JSON string escaping (newlines → \n,
// quotes → \") inflates the wire payload past the 1 MiB the agent captured, so
// a real ≥1-MiB-log completion would hit the generic 1 MiB cap and 400 (orphaning
// the job). All other routes keep the 1 MiB default via decodeJSON.
func decodeJSONLimit(r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// decodeJSONTolerant is decodeJSONLimit WITHOUT DisallowUnknownFields. Used
// by the agent-wire routes (heartbeat) where a newer agent may send fields
// an older controller does not know yet: rejecting those with a 400 would
// break rolling upgrades in the direction new-agent → old-controller, while
// unknown keys are harmless. The body cap still applies.
func decodeJSONTolerant(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- Auth middleware ----------

type ctxKey string

const nodeCtxKey ctxKey = "node"

// maxBodyBytes caps request bodies so a malicious or broken client cannot
// exhaust controller memory with oversized JSON. Applies to ALL decodeJSON
// routes except the job-complete route (see maxCompleteBodyBytes).
const maxBodyBytes = 1 << 20 // 1 MiB

// maxCompleteBodyBytes is the body cap for POST /api/agent/job/{id}/complete.
// The agent captures run.log at up to 1 MiB, but JSON string escaping
// (newlines → \n, quotes → \", control chars → \uXXXX) inflates the wire
// payload past 1 MiB; step_timings_json adds more. A 4 MiB cap accommodates
// the worst-case escaped log (each byte of a 1 MiB log becomes at most 6
// bytes via \uXXXX) plus timings and headroom, while still bounding memory.
// Only the complete route uses this; all other routes keep the 1 MiB default.
const maxCompleteBodyBytes = 4 << 20 // 4 MiB

// bearer extracts the token from an Authorization header (case-insensitive
// scheme per RFC 7235).
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// withNodeAuth resolves the node from its bearer token and injects it.
func (s *Server) withNodeAuth(h func(http.ResponseWriter, *http.Request, *model.Node)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		hash := auth.HashToken(token)
		node, err := s.Store.NodeByTokenHash(r.Context(), hash)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown node token")
			return
		}
		ctx := context.WithValue(r.Context(), nodeCtxKey, node)
		h(w, r.WithContext(ctx), node)
	}
}

// withAdmin lives in auth_handlers.go (session-based authentication).

// ---------- Health ----------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// Additive: the existing {"status":"ok"} field stays so any test/client
	// that checks for "ok" keeps passing. The "instance" field identifies
	// this replica (Phase 3 HA) so an operator can tell two controllers apart
	// behind a load balancer — both serve health, each reports its own id.
	// The "version" field exposes the controller build version (ldflags) so
	// operators can tell at a glance what's running. "oidc_enabled" (P10)
	// lets the login page decide whether to show the SSO button BEFORE any
	// session exists — health is the SPA's unauthenticated bootstrap call.
	out := map[string]any{"status": "ok", "instance": s.Cfg.InstanceID, "oidc_enabled": s.OIDC != nil}
	if s.Cfg.Version != "" {
		out["version"] = s.Cfg.Version
	}
	writeJSON(w, http.StatusOK, out)
}

// storeResolver resolves step templates from the store so rendered jobs link
// the exact PowerShell saved in the database (custom steps included).
func (s *Server) storeResolver() flow.TemplateResolver {
	return func(key string) (*model.StepTemplate, error) {
		return s.Store.StepTemplateByKey(ctxBg(), key)
	}
}

// nodeFromCtx retrieves the authenticated node.
func nodeFromCtx(r *http.Request) *model.Node {
	n, _ := r.Context().Value(nodeCtxKey).(*model.Node)
	return n
}

var errNodeNotFound = errors.New("node not found")
