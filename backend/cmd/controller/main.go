// Command controller runs the encode-system control plane: share scanner,
// job queue, agent API, and the web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/badskater/encode-system/backend/internal/api"
	"github.com/badskater/encode-system/backend/internal/scanner"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
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

	st, err := store.Open(filepath.Join(*dataDir, "encode.db"))
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

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
		AdminUsername:      adminUser,
		AdminPassword:      adminPass,
		ForceAdminPassword: os.Getenv("ENCODE_ADMIN_FORCE_PASSWORD") == "1",
		ScriptsRoot:       env("ENCODE_SCRIPTS_ROOT", filepath.Join(*dataDir, "scripts")),
		ReleaseRoot:       env("ENCODE_RELEASE_ROOT", filepath.Join(*dataDir, "release")),
		NodeBinDir:        env("ENCODE_NODE_BIN", `C:\bin`),
		NodeScriptsDir:    env("ENCODE_NODE_SCRIPTS", `C:\Encodes\scripts`),
		NodeReleaseDir:    env("ENCODE_NODE_RELEASE", `C:\Encodes\ReleaseFolders`),
		Group:             env("ENCODE_GROUP", "OldFartsSubs"),
		Tag:               env("ENCODE_TAG", "1080p"),
		DefaultFlowName:   env("ENCODE_DEFAULT_FLOW", "default-1080"),
		TasksBeforeReboot:   envInt("ENCODE_TASKS_BEFORE_REBOOT", 10),
		ScanIntervalSeconds: envInt("ENCODE_SCAN_INTERVAL", 30),
		DiscordWebhook:    env("ENCODE_DISCORD_WEBHOOK", ""),
	}

	srv, err := api.New(st, up, log, cfg)
	if err != nil {
		log.Error("init api server", "err", err)
		os.Exit(1)
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
	go scanner.RunLoop(ctx, log, st, func(scanCtx context.Context) (string, time.Duration, string) {
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
		return root, interval, defaultFlow
	})

	// Serve UI static files (built frontend) if present, then the API.
	// ENCODE_UI_DIR overrides the default <data>/ui (the Docker image bakes
	// the SPA into /app/ui, keeping it out of the persistent volume).
	mux := http.NewServeMux()
	uiDir := env("ENCODE_UI_DIR", filepath.Join(*dataDir, "ui"))
	if fi, err := os.Stat(uiDir); err == nil && fi.IsDir() {
		spa := spaHandler(uiDir)
		mux.Handle("/", spa)
	}
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
