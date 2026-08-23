// Package api implements the controller's HTTP API: agent endpoints
// (heartbeat, job lifecycle, updates) and UI endpoints (nodes, jobs, flows).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// Config carries runtime settings injected into handlers.
type Config struct {
	AdminToken        string        // UI/API admin bearer token
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
}

// Server bundles dependencies for all handlers.
type Server struct {
	Store  *store.Store
	Update *update.Store
	Log    *slog.Logger
	Cfg    Config
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
	s := &Server{Store: st, Update: up, Log: log, Cfg: cfg}
	if err := s.seedDefaultFlow(); err != nil {
		return nil, err
	}
	return s, nil
}

// seedDefaultFlow installs the standard 1080p flow on first boot.
func (s *Server) seedDefaultFlow() error {
	ctx := ctxBg()
	if _, err := s.Store.FlowByName(ctx, s.Cfg.DefaultFlowName); err == nil {
		return nil // already present
	}
	def := flow.DefaultFlow()
	def.Name = s.Cfg.DefaultFlowName
	if _, err := s.Store.CreateFlow(ctx, def); err != nil {
		return fmt.Errorf("seed default flow: %w", err)
	}
	s.Log.Info("seeded default flow", "name", def.Name, "steps", len(def.Steps))
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

	// UI endpoints — admin token auth.
	mux.HandleFunc("GET /api/nodes", s.withAdmin(s.handleListNodes))
	mux.HandleFunc("POST /api/nodes", s.withAdmin(s.handleCreateNode))
	mux.HandleFunc("PATCH /api/nodes/{id}", s.withAdmin(s.handlePatchNode))
	mux.HandleFunc("POST /api/nodes/{id}/reboot", s.withAdmin(s.handleRebootNode))

	mux.HandleFunc("GET /api/jobs", s.withAdmin(s.handleListJobs))
	mux.HandleFunc("POST /api/jobs", s.withAdmin(s.handleCreateJob))
	mux.HandleFunc("GET /api/jobs/{id}", s.withAdmin(s.handleGetJob))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.withAdmin(s.handleRetryJob))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.withAdmin(s.handleCancelJob))

	mux.HandleFunc("GET /api/flows", s.withAdmin(s.handleListFlows))
	mux.HandleFunc("POST /api/flows", s.withAdmin(s.handleCreateFlow))
	mux.HandleFunc("PUT /api/flows/{id}", s.withAdmin(s.handleUpdateFlow))
	mux.HandleFunc("DELETE /api/flows/{id}", s.withAdmin(s.handleDeleteFlow))

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

// withAdmin requires the UI admin token (constant-time compare).
func (s *Server) withAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Cfg.AdminToken == "" ||
			subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.Cfg.AdminToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		h(w, r)
	}
}

// ---------- Health ----------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// nodeFromCtx retrieves the authenticated node.
func nodeFromCtx(r *http.Request) *model.Node {
	n, _ := r.Context().Value(nodeCtxKey).(*model.Node)
	return n
}

var errNodeNotFound = errors.New("node not found")
