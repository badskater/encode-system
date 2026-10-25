// Command controller runs the encode-system control plane: share scanner,
// job queue, agent API, and the web UI.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/badskater/encode-system/backend/internal/api"
	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/provision"
	"github.com/badskater/encode-system/backend/internal/s3"
	"github.com/badskater/encode-system/backend/internal/scanner"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql (backup PG dump handle)
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("ENCODE_LISTEN", ":8080"), "listen address")
	dataDir := flag.String("data", env("ENCODE_DATA", "./data"), "data directory (db, updates, static ui)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Management-plane admin account. Username defaults to "admin". The
	// password comes from ENCODE_ADMIN_PASSWORD, but ONLY on the boot that
	// creates the account — existing accounts keep their stored bcrypt hash
	// and the env value is ignored. With no env password at all, a random
	// one is generated and logged exactly once (same boot as creation).
	adminUser := env("ENCODE_ADMIN_USER", "admin")
	adminPass := os.Getenv("ENCODE_ADMIN_PASSWORD")

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Error("create data dir", "err", err)
		os.Exit(1)
	}

	// databaseURL selects the store backend. Empty (the default) keeps the
	// historical SQLite file at <dataDir>/encode.db byte-for-byte. A
	// postgres:// DSN switches the controller to the Postgres backend
	// (store.Open → openPostgres → migratePG creates the schema fresh on a
	// new database, no-op on an existing one). store.Open already fails
	// fast (and returns a clear error) when PG is unreachable; this block
	// exits non-zero on that error, same as today's SQLite failure path.
	databaseURL := os.Getenv("ENCODE_DATABASE_URL")
	dbTarget := filepath.Join(*dataDir, "encode.db")
	if databaseURL != "" {
		dbTarget = databaseURL
	}
	st, err := store.Open(dbTarget)
	if err != nil {
		log.Error("open store", "backend", redactDSN(databaseURL), "err", err)
		os.Exit(1)
	}
	defer st.Close()
	log.Info("store backend", "backend", redactDSN(databaseURL))

	up, err := update.NewStore(filepath.Join(*dataDir, "updates"))
	if err != nil {
		log.Error("open update store", "err", err)
		os.Exit(1)
	}

	// Decide whether THIS boot will create the account; that is the only
	// situation where a generated password must be surfaced.
	adminExists := true
	if u, err := st.UserByUsername(context.Background(), adminUser); err != nil {
		log.Error("admin user lookup", "err", err)
		os.Exit(1)
	} else {
		adminExists = u != nil
	}
	generatedPass := ""
	if !adminExists && adminPass == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			log.Error("generate admin password", "err", err)
			os.Exit(1)
		}
		generatedPass = hex.EncodeToString(b)
		adminPass = generatedPass
	}

	cfg := api.Config{
		AdminUsername:       adminUser,
		AdminPassword:       adminPass,
		ForceAdminPassword:  os.Getenv("ENCODE_ADMIN_FORCE_PASSWORD") == "1",
		ScriptsRoot:         env("ENCODE_SCRIPTS_ROOT", filepath.Join(*dataDir, "scripts")),
		ReleaseRoot:         env("ENCODE_RELEASE_ROOT", filepath.Join(*dataDir, "release")),
		NodeBinDir:          env("ENCODE_NODE_BIN", `C:\bin`),
		NodeScriptsDir:      env("ENCODE_NODE_SCRIPTS", `C:\Encodes\scripts`),
		NodeReleaseDir:      env("ENCODE_NODE_RELEASE", `C:\Encodes\ReleaseFolders`),
		Group:               env("ENCODE_GROUP", "OldFartsSubs"),
		Tag:                 env("ENCODE_TAG", "1080p"),
		DefaultFlowName:     env("ENCODE_DEFAULT_FLOW", "default-1080"),
		TasksBeforeReboot:   envInt("ENCODE_TASKS_BEFORE_REBOOT", 10),
		ScanIntervalSeconds: envInt("ENCODE_SCAN_INTERVAL", 30),
		DiscordWebhook:      env("ENCODE_DISCORD_WEBHOOK", ""),
	}

	srv, err := api.New(st, up, log, cfg)
	if err != nil {
		log.Error("init api server", "err", err)
		os.Exit(1)
	}

	// Node provisioning engine: runs ansible-playbook (bundled in the image)
	// against new Windows nodes using the live settings. Playbook directory
	// is baked into the image at /app/provision by default.
	srv.Provision = &provision.Engine{
		Store:       st,
		Payloads:    &provision.UpdateStoreAdapter{Up: up},
		PlaybookDir: env("ENCODE_PROVISION_DIR", "/app/provision"),
		LogCap:      512 * 1024,
		IssuePairing: func(nameHint string) (string, error) {
			b := make([]byte, 24)
			if _, err := rand.Read(b); err != nil {
				return "", err
			}
			code := hex.EncodeToString(b)
			if _, err := st.CreatePairingCode(context.Background(), auth.HashToken(code), nameHint, time.Hour); err != nil {
				return "", err
			}
			return code, nil
		},
	}
	// Freeze-proofing: any run left queued/running by a previous process
	// (crash/restart/redeploy) is marked failed at startup instead of
	// showing "running" forever.
	if err := srv.Provision.ReconcileStaleRuns(context.Background()); err != nil {
		log.Warn("reconcile stale provision runs", "err", err)
	}
	if generatedPass != "" {
		// One-time disclosure, same boot as the account creation.
		log.Warn("GENERATED admin password (shown once; set ENCODE_ADMIN_PASSWORD to pin your own)",
			"username", adminUser, "password", generatedPass)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Scanner loop: watch the scripts share for new episode folders. The
	// root, cadence, and default flow come from the LIVE settings store so
	// Settings-page edits apply on the next cycle without a restart.
	go scanner.RunLoop(ctx, log, st, func(scanCtx context.Context) (scanner.Target, time.Duration, string) {
		root := cfg.ScriptsRoot
		interval := time.Duration(cfg.ScanIntervalSeconds) * time.Second
		defaultFlow := cfg.DefaultFlowName
		if st2, err := st.GetSettings(scanCtx); err == nil && st2 != nil {
			if st2.ScriptsRoot != "" {
				root = st2.ScriptsRoot
			}
			// Mirror validateSettings' bounds so a hand-edited DB row cannot
			// push the cadence outside [5s, 1h] even if it bypassed the API.
			if st2.ScanIntervalSeconds >= 5 && st2.ScanIntervalSeconds <= 3600 {
				interval = time.Duration(st2.ScanIntervalSeconds) * time.Second
			}
		}
		// An enabled s3 share owning the scripts role switches discovery to
		// bucket listing (full s3 autonomy: no mount on the controller).
		// Client construction is per-cycle so credential rotation applies
		// without restart; it is cheap (no network round-trip).
		if sh, err := st.ShareForRole(scanCtx, model.ShareRoleScripts); err == nil &&
			sh != nil && sh.Kind == model.ShareS3 {
			store, serr := s3.New(&model.S3Transfer{
				Endpoint:  s3.EndpointFromShare(sh),
				Region:    sh.Region,
				AccessKey: sh.Username,
				SecretKey: sh.Password,
				UseTLS:    sh.UseTLS,
			})
			if serr != nil {
				log.Warn("s3 scanner client failed; scanning disabled", "share", sh.Name, "err", serr)
				return scanner.Target{}, interval, defaultFlow
			}
			return scanner.Target{S3Bucket: sh.Path, S3Store: store}, interval, defaultFlow
		}
		return scanner.Target{Root: root}, interval, defaultFlow
	})

	// Digest loop: when settings.NotifyDigest is ON, buffer job-outcome
	// events and flush one hourly summary instead of per-job alerts. Each
	// tick reads live settings, so toggling digest on/off on the Settings
	// page applies on the next tick without a restart. Bound to the same
	// ctx as the scanner so SIGTERM stops both cleanly.
	srv.StartDigestLoop(ctx)

	// Retention loop: hourly pass that deletes terminal jobs older than
	// settings.JobRetentionDays (0 = keep forever, the default). Each tick
	// reads live settings, so enabling/adjusting retention on the Settings
	// page applies on the next tick without a restart. Bound to the same
	// ctx as the scanner/digest loops so SIGTERM stops all of them cleanly.
	srv.StartRetentionLoop(ctx)

	// DB backup scheduler: periodic VACUUM INTO snapshots under
	// <dataDir>/backups (keeps the newest 24 scheduled ones; manual
	// snapshots are never auto-pruned). Enabled/interval live-adjustable
	// via the Settings UI (PUT /api/backup/settings); env sets the boot
	// defaults. The loop polls every 30s so changes apply without a
	// restart.
	srv.Backup = &api.BackupManager{
		Store:    st,
		Postgres: databaseURL != "" && (strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://")),
		Dir:      filepath.Join(*dataDir, "backups"),
		Log:      log,
		Enabled:  env("ENCODE_BACKUP_ENABLED", "false") == "true",
		Every:    time.Duration(clampInt(envInt("ENCODE_BACKUP_EVERY_SECONDS", 21600), 3600, 86400)) * time.Second,
	}
	// On Postgres, open a second *sql.DB over the same DSN for the backup
	// dump path (catalog + SELECT * per table). The store keeps its own
	// private *sql.DB (no exported accessor), so the backup opens its own
	// pool on the same DSN — the same pattern cmd/pgimport uses. pgx stdlib
	// pools connections; the backup dump is single-threaded and reads a
	// consistent snapshot. nil on SQLite (dumpPG is never called there).
	if srv.Backup.Postgres {
		pgDump, err := sql.Open("pgx", databaseURL)
		if err != nil {
			log.Error("open pg dump handle", "err", err)
			os.Exit(1)
		}
		srv.Backup.PGDump = pgDump
	}
	// One-time migration: legacy flat NFS Settings fields become share rows
	// (idempotent; never touches existing shares).
	if n := st.MigrateSettingsShares(ctx); n > 0 {
		log.Info("migrated NFS settings into shares", "count", n)
	}

	go srv.Backup.Run(ctx)

	// Serve UI static files (built frontend) if present, then the API.
	// ENCODE_UI_DIR overrides the default <data>/ui (the Docker image bakes
	// the SPA into /app/ui, keeping it out of the persistent volume).
	mux := http.NewServeMux()
	uiDir := env("ENCODE_UI_DIR", filepath.Join(*dataDir, "ui"))
	if fi, err := os.Stat(uiDir); err == nil && fi.IsDir() {
		spa := spaHandler(uiDir)
		mux.Handle("/", spa)
	}
	// /metrics lives OUTSIDE the /api/ prefix (Prometheus convention) — mount
	// it explicitly BEFORE the SPA catch-all, or "/" would serve index.html
	// to scrapers. Routes() contains the "GET /metrics" pattern, and a mux
	// mounted at "/metrics" still sees the full path, so it matches.
	mux.Handle("/metrics", srv.Routes())
	mux.Handle("/api/", srv.Routes())

	httpSrv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("controller starting", "listen", *listen, "data", *dataDir,
		"scripts_root", cfg.ScriptsRoot, "scan_interval_s", cfg.ScanIntervalSeconds)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	log.Info("controller stopped")
}

// redactDSN turns a database target into a safe, secret-free label for logs.
//
// Empty (SQLite default) → "sqlite". A non-empty value that is not a
// postgres:// URL → "sqlite:<basename>" (the file path, basename only — a
// full path could leak a host directory layout). A postgres:// URL →
// "postgres://<host>/<dbname>", with the userinfo (user:password) stripped:
// only scheme + host + db name survive, never the password. A DSN that fails
// to parse → "postgres:<redacted>" so the password is never logged even on a
// malformed input. The same function is used on the success log line (one
// line, which backend is active) and the failure line (which backend failed).
func redactDSN(dbTarget string) string {
	if dbTarget == "" {
		return "sqlite"
	}
	if !strings.HasPrefix(dbTarget, "postgres://") && !strings.HasPrefix(dbTarget, "postgresql://") {
		return "sqlite:" + filepath.Base(dbTarget)
	}
	u, err := url.Parse(dbTarget)
	if err != nil || u.Host == "" {
		return "postgres:<redacted>"
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		db = "?"
	}
	return u.Scheme + "://" + u.Host + "/" + db
}

// clampInt bounds v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// envInt parses an integer env var with a fallback.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// spaHandler serves static files, falling back to index.html for client routes.
func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean(r.URL.Path))
		if _, err := os.Stat(p); err != nil {
			r.URL.Path = "/" // SPA fallback
		}
		fs.ServeHTTP(w, r)
	})
}
