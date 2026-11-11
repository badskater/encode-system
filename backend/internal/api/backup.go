package api

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/store"
)

// maxBackups is how many snapshots the scheduled job keeps (oldest are
// pruned after each run). Manual triggers are NOT auto-pruned — an operator
// taking a snapshot before a risky deploy shouldn't have it silently
// deleted an hour later; they list and delete explicitly.
const maxBackups = 24

// exitFn is os.Exit by default; overridden in tests so the SQLite restore
// path (which exits the process so Docker's restart policy reboots on the
// restored DB) doesn't kill the test binary.
var exitFn = os.Exit

// BackupManager owns DB snapshot scheduling. Snapshots live in
// <dataDir>/backups. On SQLite they are VACUUM INTO .db files; on Postgres
// they are logical table-walk JSON-lines .dump files (see dumpPG).
type BackupManager struct {
	Store *store.Store
	// Postgres selects the snapshot strategy: true → logical JSON dump
	// (dumpPG), false → VACUUM INTO (.db). Set by the controller from the
	// ENCODE_DATABASE_URL env (a postgres:// DSN ⇒ true). The store package
	// keeps its own isPostgres() private, so the controller — which already
	// knows the backend it selected — passes the flag in here.
	Postgres bool
	// PGDump is the *sql.DB the PG dump path uses for catalog/SELECT queries.
	// It must be the same pool the store uses (store.Open's pgx handle) so the
	// dump reads a consistent snapshot of the live schema. nil on SQLite.
	PGDump  *sql.DB
	Dir     string // absolute path to the backups dir
	Log     *slog.Logger
	Enabled bool // scheduled snapshots on/off (manual trigger always works)
	Every   time.Duration

	lastRun   time.Time
	lastError string
	nextRun   time.Time

	// AfterSnapshot, when non-nil, runs after EVERY successful snapshot —
	// scheduled and manual alike. It cannot fail the backup: the local
	// snapshot is already durable when it fires, and the hook returns
	// nothing. Production wires the offsite upload here (P4); tests inject
	// a recorder. The hook gets a COPY of the info and a background ctx.
	AfterSnapshot func(ctx context.Context, info *BackupInfo)
	// afterSnapshotGo launches the hook. Default (nil): a goroutine, so
	// remote uploads never block the scheduler tick or the manual-backup
	// HTTP response. Tests override it to run synchronously. Same injectable
	// seam pattern as exitFn.
	afterSnapshotGo func(fn func())
}

// BackupInfo describes one snapshot for the UI/API.
type BackupInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	Scheduled bool      `json:"scheduled"`
}

// TakeSnapshot writes a new snapshot with a timestamped name. On SQLite it
// uses VACUUM INTO (a standalone .db file). On Postgres it writes a logical
// table-walk dump as newline-delimited JSON (one JSON object per row, grouped
// by table, in FK order) — a .dump file the operator can re-import with
// pgimport -force or restore via a COPY/INSERT path. scheduled=true marks it
// for auto-pruning; manual snapshots are kept until explicitly deleted.
// Returns the created file's info.
//
// No-secrets contract (verified to match the SQLite VACUUM INTO parity): the
// dump includes the shares table's password column exactly as SQLite VACUUM
// INTO would — i.e. the AES-GCM ciphertext ("v1:<hex>"), NEVER plaintext and
// NEVER the key file. The share-credential encryption key lives BESIDE the DB
// (<sqlite>.shares-key) and is not inside it; a SQLite VACUUM INTO backup
// therefore already carries the ciphertext-but-not-the-key, and the PG dump
// preserves that identical contract. The key file must be backed up
// separately (out of band) for a restore to decrypt.
func (b *BackupManager) TakeSnapshot(ctx context.Context, scheduled bool) (*BackupInfo, error) {
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create backups dir: %w", err)
	}
	kind := "manual"
	if scheduled {
		kind = "sched"
	}
	pg := b.Postgres
	suffix := ".db"
	if pg {
		suffix = ".dump"
	}
	name := fmt.Sprintf("encode-%s-%s%s", time.Now().UTC().Format("20060102-150405"), kind, suffix)
	dest := filepath.Join(b.Dir, name)
	// A name collision means two snapshots in the same second — append a
	// counter rather than failing the backup.
	for i := 1; fileExists(dest); i++ {
		dest = filepath.Join(b.Dir, fmt.Sprintf("encode-%s-%s-%d%s",
			time.Now().UTC().Format("20060102-150405"), kind, i, suffix))
	}
	if pg {
		if err := b.dumpPG(ctx, dest); err != nil {
			return nil, fmt.Errorf("pg dump: %w", err)
		}
	} else {
		if err := b.Store.BackupTo(ctx, dest); err != nil {
			return nil, fmt.Errorf("vacuum into: %w", err)
		}
	}
	info0, err := os.Stat(dest)
	if err != nil {
		return nil, err
	}
	if scheduled {
		b.pruneScheduled(ctx)
	}
	// Reflect the actual filename (counter suffix may have advanced it).
	base := filepath.Base(dest)
	info := &BackupInfo{Name: base, SizeBytes: info0.Size(), CreatedAt: info0.ModTime().UTC(), Scheduled: scheduled}
	// Offsite hook (P4): fires for scheduled AND manual snapshots, after
	// the local file is durable. Launch is a seam so tests run it
	// synchronously; production detaches so remote I/O never blocks the
	// scheduler or the HTTP response. Capture the hook + info copy NOW —
	// the delayed goroutine must not read fields that tests may restore.
	if b.AfterSnapshot != nil {
		hook, snap := b.AfterSnapshot, *info
		launch := b.afterSnapshotGo
		if launch == nil {
			launch = func(fn func()) { go fn() }
		}
		launch(func() { hook(ctxBg(), &snap) })
	}
	return info, nil
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
		if e.IsDir() {
			continue
		}
		// SQLite snapshots are .db (VACUUM INTO); PG snapshots are .dump
		// (logical JSON-lines dump). Both are valid download/delete targets.
		if !strings.HasSuffix(e.Name(), ".db") && !strings.HasSuffix(e.Name(), ".dump") {
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

// Delete removes one snapshot by exact filename. Only plain .db/.dump names
// without path separators are accepted (no traversal).
func (b *BackupManager) Delete(name string) error {
	if name == "" || strings.ContainsAny(name, `/\\`) ||
		(!strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".dump")) {
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
	if name == "" || strings.ContainsAny(name, `/\\`) ||
		(!strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".dump")) {
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

// ---------- Backup restore ----------

// handleBackupRestore: POST /api/backup/{name}/restore — admin-only.
//
// Guards:
//   - name must be a plain filename (no path separators, no "..") and must
//     exist in the backups dir
//   - extension must match the active backend (.db for SQLite, .dump for PG)
//   - any pending OR running job → 409 "cancel or drain active jobs first"
//
// SQLite: snapshots the current DB to encode.db.pre-restore-<unix-ts>, then
// copies the backup to a temp file and atomically renames it over the live
// db file. The shares-key file is never touched. Responds 200 with
// {"restored":true,"restart":true}, then exits the process (Docker restart
// policy reboots on the restored DB). exitFn is injectable for tests.
//
// PG: replays the dump in one transaction (TRUNCATE in reverse FK order,
// INSERT per row, setval per serial). Responds 200 with
// {"restored":true,"restart":false} — no restart needed.
//
// Audit backup.restore BEFORE the swap so the audit row survives even if
// the process exits immediately after.
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "backups unavailable")
		return
	}
	name := r.PathValue("name")
	// Path traversal guard: name must be a plain filename.
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeErr(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	// Extension must be valid.
	if !strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".dump") {
		writeErr(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	// Existence check.
	srcPath := filepath.Join(s.Backup.Dir, name)
	if !fileExists(srcPath) {
		writeErr(w, http.StatusNotFound, "backup not found")
		return
	}
	// Extension must match the active backend.
	wantSuffix := ".db"
	if s.Backup.Postgres {
		wantSuffix = ".dump"
	}
	if !strings.HasSuffix(name, wantSuffix) {
		writeErr(w, http.StatusConflict, "backup extension does not match active database backend")
		return
	}
	// Active-job guard: a restore mid-flight would lose in-flight work.
	active, err := s.Store.CountActiveJobs(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "check active jobs")
		return
	}
	if active > 0 {
		writeErr(w, http.StatusConflict, "cancel or drain active jobs first")
		return
	}
	// Audit BEFORE the swap (the process may exit immediately after on SQLite).
	s.audit(r, "backup.restore", "db", map[string]string{"name": name})

	if s.Backup.Postgres {
		if err := s.restorePG(r.Context(), srcPath); err != nil {
			s.Log.Error("PG restore failed", "err", err, "name", name)
			writeErr(w, http.StatusInternalServerError, "restore failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"restored": true, "restart": false})
		return
	}

	// SQLite: snapshot current DB, then swap.
	dbPath := s.Store.DBPath()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	snapPath := dbPath + ".pre-restore-" + ts
	if err := copyFile(dbPath, snapPath); err != nil {
		s.Log.Error("snapshot before restore failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "pre-restore snapshot failed: "+err.Error())
		return
	}
	// Copy backup → temp, then atomic rename over the live db file.
	// os.Rename requires same filesystem; the backups dir is under the same
	// data dir, so this is safe. A temp file beside the db avoids cross-FS.
	tmpPath := dbPath + ".restore-tmp"
	if err := copyFile(srcPath, tmpPath); err != nil {
		s.Log.Error("copy backup to temp failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "restore copy failed: "+err.Error())
		return
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		s.Log.Error("rename over live db failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "restore swap failed: "+err.Error())
		return
	}
	// Respond first, then exit so Docker's restart policy reboots on the
	// restored DB. The response is flushed before exit.
	s.Log.Info("SQLite DB restored from backup; exiting for restart", "name", name, "snapshot", snapPath)
	writeJSON(w, http.StatusOK, map[string]any{"restored": true, "restart": true, "snapshot": filepath.Base(snapPath)})
	// Flush the response before exiting. The statusRecorder/WriteHeader path
	// already wrote the JSON to the wire via writeJSON; give the writer a
	// final flush if it supports it.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Capture the exit function NOW, not when the delayed goroutine runs:
	// tests override exitFn and restore it in cleanup, and a 200ms-delayed
	// read could observe the real os.Exit after cleanup already ran (killed
	// the test binary in CI).
	exit := exitFn
	go func() {
		time.Sleep(200 * time.Millisecond) // let the response hit the wire
		s.Log.Info("controller exiting after DB restore (Docker restart policy reboots on restored DB)")
		exit(0)
	}()
}

// copyFile copies src to dst, creating or truncating dst. Used by the
// SQLite restore path for the pre-restore snapshot and the backup→temp copy.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
