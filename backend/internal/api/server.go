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
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"golang.org/x/crypto/bcrypt"
	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// Config carries runtime settings injected into handlers.
type Config struct {
	AdminUsername     string        // management-plane admin account (seeded at startup)
	AdminPassword     string        // initial password for the admin account (only used when the account doesn't exist)
	ScriptsRoot       string        // controller-side scripts share mount
	ReleaseRoot       string        // controller-side release share mount
	NodeBinDir        string        // tools dir on nodes, e.g. C:\bin
	NodeScriptsDir    string        // scripts mount on nodes, e.g. C:\Encodes\scripts
	NodeReleaseDir    string        // release mount on nodes
	Group             string        // release group tag
	Tag               string        // quality tag, e.g. 1080p
	TasksBeforeReboot int           // reboot threshold (default 10)
	RebootGracePeriod time.Duration // reboot attempt expires after this (default 10m)
	StaleAfter        time.Duration // node offline after no heartbeat this long
	DefaultFlowName   string        // flow used for auto-created jobs
	DiscordWebhook    string        // optional job-outcome alerts (empty = off)
}

// Server bundles dependencies for all handlers.
type Server struct {
	Store    *store.Store
	Update   *update.Store
	Log      *slog.Logger
	Cfg      Config
	Notify   notify.Notifier
	throttle *loginThrottle
}

// New builds the server and seeds the default flow when absent.
func New(st *store.Store, up *update.Store, log *slog.Logger, cfg Config) (*Server, error) {
	if cfg.TasksBeforeReboot <= 0 {
		cfg.TasksBeforeReboot = 10
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
	s := &Server{Store: st, Update: up, Log: log, Cfg: cfg, Notify: notify.NewDiscord(cfg.DiscordWebhook, log), throttle: &loginThrottle{}}
	if cfg.DiscordWebhook != "" {
		log.Info("discord notifications enabled")
	}
	if err := s.seedStepTemplates(); err != nil {
		return nil, err
	}
	if err := s.seedDefaultFlow(); err != nil {
		return nil, err
	}
	if err := s.seedAdminUser(); err != nil {
		return nil, err
	}
	return s, nil
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
	return nil
}

// seedDefaultFlow installs the standard 1080p flow on first boot and ensures
// exactly one flow carries the default flag. When no flow is default yet
// (fresh installs and databases created before the flag existed), the
// configured default flow takes the flag.
func (s *Server) seedDefaultFlow() error {
	ctx := ctxBg()
	if _, err := s.Store.FlowByName(ctx, s.Cfg.DefaultFlowName); err != nil {
		def := flow.DefaultFlow()
		def.Name = s.Cfg.DefaultFlowName
		if _, err := s.Store.CreateFlow(ctx, def); err != nil {
			return fmt.Errorf("seed default flow: %w", err)
		}
		s.Log.Info("seeded default flow", "name", def.Name, "steps", len(def.Steps))
	}
	if _, err := s.Store.DefaultFlow(ctx); err != nil {
		fl, err := s.Store.FlowByName(ctx, s.Cfg.DefaultFlowName)
		if err != nil {
			return fmt.Errorf("resolve default flow: %w", err)
		}
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
	mux.HandleFunc("POST /api/auth/logout", s.withAdmin(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.withAdmin(s.handleMe))

	// UI endpoints — session auth.
	mux.HandleFunc("GET /api/nodes", s.withAdmin(s.handleListNodes))
	mux.HandleFunc("POST /api/nodes", s.withAdmin(s.handleCreateNode))
	mux.HandleFunc("PATCH /api/nodes/{id}", s.withAdmin(s.handlePatchNode))
	mux.HandleFunc("POST /api/nodes/{id}/reboot", s.withAdmin(s.handleRebootNode))

	mux.HandleFunc("GET /api/jobs", s.withAdmin(s.handleListJobs))
	mux.HandleFunc("POST /api/jobs", s.withAdmin(s.handleCreateJob))
	mux.HandleFunc("GET /api/jobs/{id}", s.withAdmin(s.handleGetJob))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.withAdmin(s.handleRetryJob))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.withAdmin(s.handleCancelJob))
	mux.HandleFunc("PATCH /api/jobs/{id}", s.withAdmin(s.handlePatchJob))

	mux.HandleFunc("GET /api/flows", s.withAdmin(s.handleListFlows))
	mux.HandleFunc("POST /api/flows", s.withAdmin(s.handleCreateFlow))
	mux.HandleFunc("PUT /api/flows/{id}", s.withAdmin(s.handleUpdateFlow))
	mux.HandleFunc("DELETE /api/flows/{id}", s.withAdmin(s.handleDeleteFlow))
	mux.HandleFunc("POST /api/flows/{id}/default", s.withAdmin(s.handleSetDefaultFlow))
	mux.HandleFunc("GET /api/flows/{id}/export", s.withAdmin(s.handleExportFlow))
	mux.HandleFunc("POST /api/flows/import", s.withAdmin(s.handleImportFlow))

	mux.HandleFunc("GET /api/series", s.withAdmin(s.handleListSeries))
	mux.HandleFunc("PATCH /api/series/{id}", s.withAdmin(s.handlePatchSeries))

	mux.HandleFunc("GET /api/step-templates", s.withAdmin(s.handleListStepTemplates))
	mux.HandleFunc("POST /api/step-templates", s.withAdmin(s.handleCreateStepTemplate))
	mux.HandleFunc("PUT /api/step-templates/{id}", s.withAdmin(s.handleUpdateStepTemplate))
	mux.HandleFunc("DELETE /api/step-templates/{id}", s.withAdmin(s.handleDeleteStepTemplate))
	mux.HandleFunc("POST /api/step-templates/{id}/reset", s.withAdmin(s.handleResetStepTemplate))

	mux.HandleFunc("GET /api/pairing", s.withAdmin(s.handleListPairingCodes))
	mux.HandleFunc("POST /api/pairing", s.withAdmin(s.handleCreatePairingCode))
	mux.HandleFunc("POST /api/agent/pair", s.handleAgentPair)

	mux.HandleFunc("GET /api/settings", s.withAdmin(s.handleGetSettings))

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
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ---------- Auth middleware ----------

type ctxKey string

const nodeCtxKey ctxKey = "node"

// maxBodyBytes caps request bodies so a malicious or broken client cannot
// exhaust controller memory with oversized JSON.
const maxBodyBytes = 1 << 20 // 1 MiB

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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
