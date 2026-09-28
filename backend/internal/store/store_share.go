package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- shares ----------

// ensureSharesTable creates the shares table (idempotent; called from
// migrateV2 so every Open gets it).
func (s *Store) ensureSharesTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS shares (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  role TEXT NOT NULL,
  server TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  username TEXT NOT NULL DEFAULT '',
  password TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  use_tls INTEGER NOT NULL DEFAULT 0,
  endpoint TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  mount_path TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
)`)
	return err
}

// shareKeyPath is the encryption-key file for share credentials. It lives
// BESIDE the DB (not inside it) on purpose: the backup-download endpoint
// hands the DB file to admins, and that must not hand over share
// credentials too.
func (s *Store) shareKeyPath() string { return s.dbPath + ".shares-key" }

// shareCipher returns (creating on first use) the AES-GCM cipher protecting
// share passwords. Key is 32 random bytes, stored 0600.
func (s *Store) shareCipher() (cipher.AEAD, error) {
	if s.aead != nil {
		return s.aead, nil
	}
	key, err := os.ReadFile(s.shareKeyPath())
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate share key: %w", err)
		}
		if err := os.WriteFile(s.shareKeyPath(), key, 0o600); err != nil {
			return nil, fmt.Errorf("persist share key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read share key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s.aead = aead
	return aead, nil
}

// encryptSharePassword returns a "v1:<hex>" ciphertext (empty stays empty).
func (s *Store) encryptSharePassword(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	aead, err := s.shareCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, []byte(plain), nil)
	return "v1:" + hex.EncodeToString(ct), nil
}

// decryptSharePassword reverses encryptSharePassword. Values without the
// v1: prefix are treated as legacy plaintext (defensive: none exist yet).
func (s *Store) decryptSharePassword(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, "v1:") {
		return stored, nil
	}
	raw, err := hex.DecodeString(stored[3:])
	if err != nil {
		return "", err
	}
	aead, err := s.shareCipher()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("share ciphertext too short")
	}
	nonce, ct := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

const shareCols = `id, name, kind, role, server, path, port, username, password,
  region, use_tls, endpoint, enabled, mount_path, created_at, updated_at`

// CreateShare inserts a share, encrypting Password at rest. The returned
// struct keeps the plaintext password (the caller just supplied it).
func (s *Store) CreateShare(ctx context.Context, sh *model.Share) (*model.Share, error) {
	if strings.TrimSpace(sh.Name) == "" {
		return nil, errors.New("share name required")
	}
	switch sh.Kind {
	case model.ShareNFS, model.ShareSMB, model.ShareS3:
	default:
		return nil, fmt.Errorf("unknown share kind %q", sh.Kind)
	}
	switch sh.Role {
	case model.ShareRoleScripts, model.ShareRoleRelease:
	default:
		return nil, fmt.Errorf("unknown share role %q", sh.Role)
	}
	ct, err := s.encryptSharePassword(sh.Password)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO shares (name, kind, role, server, path, port, username, password,
  region, use_tls, endpoint, enabled, mount_path)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sh.Name, sh.Kind, sh.Role, sh.Server, sh.Path, sh.Port, sh.Username, ct,
		sh.Region, boolInt(sh.UseTLS), sh.Endpoint, boolInt(sh.Enabled), sh.MountPath)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	sh.ID = id
	now := time.Now().UTC()
	sh.CreatedAt, sh.UpdatedAt = now, now
	return sh, nil
}

// GetShare loads one share with the password decrypted.
func (s *Store) GetShare(ctx context.Context, id int64) (*model.Share, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+shareCols+" FROM shares WHERE id=?", id)
	return s.scanShareRow(row)
}

// scanShareRow scans one row and decrypts the password.
func (s *Store) scanShareRow(row *sql.Row) (*model.Share, error) {
	var sh model.Share
	var kind, role, pw, created, updated string
	var useTLS, enabled int
	if err := row.Scan(&sh.ID, &sh.Name, &kind, &role, &sh.Server, &sh.Path, &sh.Port,
		&sh.Username, &pw, &sh.Region, &useTLS, &sh.Endpoint, &enabled, &sh.MountPath,
		&created, &updated); err != nil {
		return nil, err
	}
	sh.Kind = model.ShareKind(kind)
	sh.Role = model.ShareRole(role)
	sh.UseTLS = useTLS == 1
	sh.Enabled = enabled == 1
	sh.CreatedAt = parseTime(created)
	sh.UpdatedAt = parseTime(updated)
	plain, err := s.decryptSharePassword(pw)
	if err != nil {
		return nil, fmt.Errorf("decrypt share %d password: %w", sh.ID, err)
	}
	sh.Password = plain
	return &sh, nil
}

// ListShares returns all shares ordered by role,kind,name with passwords
// decrypted (provisioning needs them; the API layer masks).
func (s *Store) ListShares(ctx context.Context) ([]*model.Share, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+shareCols+" FROM shares ORDER BY role, kind, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Share
	for rows.Next() {
		var sh model.Share
		var kind, role, pw, created, updated string
		var useTLS, enabled int
		if err := rows.Scan(&sh.ID, &sh.Name, &kind, &role, &sh.Server, &sh.Path, &sh.Port,
			&sh.Username, &pw, &sh.Region, &useTLS, &sh.Endpoint, &enabled, &sh.MountPath,
			&created, &updated); err != nil {
			return nil, err
		}
		sh.Kind = model.ShareKind(kind)
		sh.Role = model.ShareRole(role)
		sh.UseTLS = useTLS == 1
		sh.Enabled = enabled == 1
		sh.CreatedAt = parseTime(created)
		sh.UpdatedAt = parseTime(updated)
		plain, err := s.decryptSharePassword(pw)
		if err != nil {
			return nil, fmt.Errorf("decrypt share %d password: %w", sh.ID, err)
		}
		sh.Password = plain
		out = append(out, &sh)
	}
	return out, rows.Err()
}

// ShareForRole returns the enabled share serving a role, preferring SMB
// over NFS over S3 for mount-based consumers (deterministic when several
// exist). Nil,nil when none.
func (s *Store) ShareForRole(ctx context.Context, role model.ShareRole) (*model.Share, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT `+shareCols+` FROM shares WHERE role=? AND enabled=1
ORDER BY CASE kind WHEN 'smb' THEN 0 WHEN 'nfs' THEN 1 ELSE 2 END, id
LIMIT 1`, role)
	sh, err := s.scanShareRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sh, err
}

// UpdateShare applies field changes. An empty Password keeps the stored
// one (the API never round-trips plaintext); to clear it set a sentinel
// via DeleteShare/recreate — passwords are only ever replaced, not wiped.
func (s *Store) UpdateShare(ctx context.Context, sh *model.Share) error {
	pwSQL := "password=password"
	args := []any{sh.Name, sh.Kind, sh.Role, sh.Server, sh.Path, sh.Port,
		sh.Username, sh.Region, boolInt(sh.UseTLS), sh.Endpoint,
		boolInt(sh.Enabled), sh.MountPath}
	if sh.Password != "" {
		ct, err := s.encryptSharePassword(sh.Password)
		if err != nil {
			return err
		}
		pwSQL = "password=?"
		// password arg slot goes right after username to match the SET order
		args = []any{sh.Name, sh.Kind, sh.Role, sh.Server, sh.Path, sh.Port,
			sh.Username, ct, sh.Region, boolInt(sh.UseTLS), sh.Endpoint,
			boolInt(sh.Enabled), sh.MountPath}
	}
	q := `UPDATE shares SET name=?, kind=?, role=?, server=?, path=?, port=?, username=?, ` +
		pwSQL + `, region=?, use_tls=?, endpoint=?, enabled=?, mount_path=?,
  updated_at=datetime('now') WHERE id=?`
	args = append(args, sh.ID)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteShare removes a share row.
func (s *Store) DeleteShare(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM shares WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MigrateSettingsShares converts the legacy flat NFS Settings fields into
// share rows on first boot after upgrade. Runs once per unique
// (server,path); returns how many rows were created. Never destructive:
// the Settings fields stay readable for the old code paths until they're
// fully removed.
func (s *Store) MigrateSettingsShares(ctx context.Context) int {
	st, err := s.GetSettings(ctx)
	if err != nil || st == nil {
		return 0
	}
	server := strings.TrimSpace(st.NFSServer)
	if server == "" {
		return 0
	}
	created := 0
	for _, m := range []struct {
		role model.ShareRole
		path string
		name string
	}{
		{model.ShareRoleScripts, strings.TrimSpace(st.ScriptsShare), "nfs-scripts"},
		{model.ShareRoleRelease, strings.TrimSpace(st.ReleaseShare), "nfs-release"},
	} {
		if m.path == "" {
			continue
		}
		var exists int
		s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM shares WHERE server=? AND path=?", server, m.path).Scan(&exists)
		if exists > 0 {
			continue
		}
		if _, err := s.CreateShare(ctx, &model.Share{
			Name: m.name, Kind: model.ShareNFS, Role: m.role,
			Server: server, Path: m.path, Enabled: true,
		}); err == nil {
			created++
		}
	}
	return created
}

// boolInt maps a bool to SQLite's 0/1.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
