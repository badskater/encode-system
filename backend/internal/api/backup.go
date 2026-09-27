package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/store"
)

// maxBackups is how many snapshots the scheduled job keeps (oldest are
// pruned after each run). Manual triggers are NOT auto-pruned — an operator
// taking a snapshot before a risky deploy shouldn't have it silently
// deleted an hour later; they list and delete explicitly.
const maxBackups = 24

// BackupManager owns DB snapshot scheduling. Snapshots live in
// <dataDir>/backups as encode-<UTC timestamp>.db (VACUUM INTO output).
type BackupManager struct {
	Store   *store.Store
	Dir     string // absolute path to the backups dir
	Log     *slog.Logger
	Enabled bool // scheduled snapshots on/off (manual trigger always works)
	Every   time.Duration

	lastRun   time.Time
	lastError string
	nextRun   time.Time
}

// BackupInfo describes one snapshot for the UI/API.
type BackupInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	Scheduled bool      `json:"scheduled"`
}

// TakeSnapshot writes a new VACUUM INTO snapshot with a timestamped name.
// scheduled=true marks it for auto-pruning; manual snapshots are kept until
// explicitly deleted. Returns the created file's info.
func (b *BackupManager) TakeSnapshot(ctx context.Context, scheduled bool) (*BackupInfo, error) {
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create backups dir: %w", err)
	}
	kind := "manual"
	if scheduled {
		kind = "sched"
	}
	name := fmt.Sprintf("encode-%s-%s.db", time.Now().UTC().Format("20060102-150405"), kind)
	dest := filepath.Join(b.Dir, name)
	// A name collision means two snapshots in the same second — append a
	// counter rather than failing the backup.
	for i := 1; fileExists(dest); i++ {
		dest = filepath.Join(b.Dir, fmt.Sprintf("encode-%s-%s-%d.db",
			time.Now().UTC().Format("20060102-150405"), kind, i))
	}
	if err := b.Store.BackupTo(ctx, dest); err != nil {
		return nil, fmt.Errorf("vacuum into: %w", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return nil, err
	}
	if scheduled {
		b.pruneScheduled(ctx)
	}
	return &BackupInfo{Name: name, SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC(), Scheduled: scheduled}, nil
}

// List returns all snapshots newest-first.
func (b *BackupManager) List() ([]*BackupInfo, error) {
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*BackupInfo{}, nil
		}
		return nil, err
	}
	out := []*BackupInfo{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, &BackupInfo{
			Name:      e.Name(),
			SizeBytes: info.Size(),
			CreatedAt: info.ModTime().UTC(),
			Scheduled: strings.Contains(e.Name(), "-sched"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Delete removes one snapshot by exact filename. Only plain .db names
// without path separators are accepted (no traversal).
func (b *BackupManager) Delete(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".db") {
		return fmt.Errorf("invalid backup name")
	}
	path := filepath.Join(b.Dir, name)
	if !fileExists(path) {
		return os.ErrNotExist
	}
	return os.Remove(path)
}

// pruneScheduled keeps at most maxBackups scheduled snapshots (oldest first
// to go). Manual snapshots are never touched.
func (b *BackupManager) pruneScheduled(ctx context.Context) {
	list, err := b.List()
	if err != nil {
		b.Log.Warn("backup prune: list", "err", err)
		return
	}
	sched := []*BackupInfo{}
	for _, bi := range list {
		if bi.Scheduled {
			sched = append(sched, bi)
		}
	}
	// list is newest-first; delete the tail beyond the cap.
	for i := maxBackups; i < len(sched); i++ {
		if err := b.Delete(sched[i].Name); err != nil {
			b.Log.Warn("backup prune: delete", "name", sched[i].Name, "err", err)
			continue
		}
		b.Log.Info("backup pruned", "name", sched[i].Name)
	}
	_ = ctx
}

// Status reports the schedule state for the UI.
func (b *BackupManager) Status() map[string]any {
	out := map[string]any{
		"enabled":       b.Enabled,
		"every_seconds": int(b.Every.Seconds()),
		"max_backups":   maxBackups,
	}
	if !b.lastRun.IsZero() {
		out["last_run"] = b.lastRun.UTC().Format(time.RFC3339)
	}
	if b.lastError != "" {
		out["last_error"] = b.lastError
	}
	if b.Enabled && !b.nextRun.IsZero() {
		out["next_run"] = b.nextRun.UTC().Format(time.RFC3339)
	}
	return out
}

// Run starts the scheduled snapshot loop. Stops when ctx is cancelled.
// Polls every 30s rather than using time.Ticker(Every) so live changes to
// Enabled/EverySeconds via PUT /api/backup/settings take effect without a
// controller restart.
func (b *BackupManager) Run(ctx context.Context) {
	b.nextRun = time.Now().Add(b.Every).UTC()
	b.Log.Info("DB backup scheduler started", "every", b.Every.String(), "keep", maxBackups)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !b.Enabled {
				// Keep nextRun ahead of now so re-enabling doesn't fire
				// immediately — the next slot is a full interval away.
				b.nextRun = now.Add(b.Every).UTC()
				continue
			}
			if now.Before(b.nextRun) {
				continue
			}
			info, err := b.TakeSnapshot(ctx, true)
			b.lastRun = time.Now().UTC()
			b.nextRun = b.lastRun.Add(b.Every)
			if err != nil {
				b.lastError = err.Error()
				b.Log.Error("scheduled DB backup failed", "err", err)
				continue
			}
			b.lastError = ""
			b.Log.Info("scheduled DB backup taken", "name", info.Name, "bytes", info.SizeBytes)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---------- HTTP handlers ----------

// handleBackupStatus: GET /api/backup — schedule state + snapshot list.
func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backups unavailable")
		return
	}
	list, err := s.Backup.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list backups")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    s.Backup.Status(),
		"snapshots": list,
	})
}

// handleBackupNow: POST /api/backup — take a manual snapshot immediately.
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backups unavailable")
		return
	}
	info, err := s.Backup.TakeSnapshot(r.Context(), false)
	if err != nil {
		s.Log.Error("manual backup failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "backup failed: "+err.Error())
		return
	}
	s.audit(r, "backup.take", "db", map[string]any{"name": info.Name, "bytes": info.SizeBytes})
	s.Log.Info("manual DB backup taken", "name", info.Name, "bytes", info.SizeBytes)
	writeJSON(w, http.StatusCreated, info)
}

// handleBackupDownload: GET /api/backup/{name} — stream a snapshot file.
// Content-Disposition attachment so browsers save instead of render.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backups unavailable")
		return
	}
	name := r.PathValue("name")
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".db") {
		writeErr(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	path := filepath.Join(s.Backup.Dir, name)
	if !fileExists(path) {
		writeErr(w, http.StatusNotFound, "backup not found")
		return
	}
	s.audit(r, "backup.download", "db", map[string]any{"name": name})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

// handleBackupDelete: DELETE /api/backup/{name}.
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backups unavailable")
		return
	}
	name := r.PathValue("name")
	if err := s.Backup.Delete(name); err != nil {
		if err == os.ErrNotExist {
			writeErr(w, http.StatusNotFound, "backup not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "backup.delete", "db", map[string]any{"name": name})
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateBackupSettings: PUT /api/backup/settings — toggle/interval.
func (s *Server) handleUpdateBackupSettings(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backups unavailable")
		return
	}
	var req struct {
		Enabled      *bool `json:"enabled"`
		EverySeconds *int  `json:"every_seconds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Enabled != nil {
		s.Backup.Enabled = *req.Enabled
	}
	if req.EverySeconds != nil {
		// Bounds: hourly-ish to daily. Below an hour spams disk for little
		// gain (VACUUM INTO rewrites the whole DB); the operator can always
		// take manual snapshots.
		if *req.EverySeconds < 3600 || *req.EverySeconds > 86400 {
			writeErr(w, http.StatusBadRequest, "every_seconds must be 3600-86400")
			return
		}
		s.Backup.Every = time.Duration(*req.EverySeconds) * time.Second
	}
	s.audit(r, "backup.settings", "db", map[string]any{
		"enabled": s.Backup.Enabled, "every_seconds": int(s.Backup.Every.Seconds()),
	})
	writeJSON(w, http.StatusOK, s.Backup.Status())
}
