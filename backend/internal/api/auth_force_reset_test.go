package api

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// TestForceAdminPasswordRecoveryHatch verifies the lost-password recovery:
// with the force flag set, the existing account's hash is overwritten from
// the env password; without the flag, env is ignored for existing accounts.
func TestForceAdminPasswordRecoveryHatch(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	mk := func(pass string, force bool) error {
		_, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
			AdminUsername: "admin", AdminPassword: pass, ForceAdminPassword: force,
			ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		})
		return err
	}

	// Boot 1: create account with "original-pass-1".
	if err := mk("original-pass-1", false); err != nil {
		t.Fatal(err)
	}

	// Boot 2: different env password WITHOUT force — must be ignored.
	if err := mk("attacker-pass-99", false); err != nil {
		t.Fatal(err)
	}
	if pwOK(t, st, "admin", "attacker-pass-99") {
		t.Fatal("env password changed the account without the force flag")
	}
	if !pwOK(t, st, "admin", "original-pass-1") {
		t.Fatal("original password broken by non-forced boot")
	}

	// Mint a session before the force reset: it must NOT survive (a reset
	// may follow a compromise; stale/stolen sessions must die).
	u, err := st.UserByUsername(ctxBg(), "admin")
	if err != nil || u == nil {
		t.Fatal("admin lookup failed")
	}
	staleTok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctxBg(), auth.HashToken(staleTok), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Boot 3: force flag set — hash overwritten with the env password.
	if err := mk("recovery-pass-42", true); err != nil {
		t.Fatal(err)
	}
	if !pwOK(t, st, "admin", "recovery-pass-42") {
		t.Fatal("forced recovery password not applied")
	}
	if pwOK(t, st, "admin", "original-pass-1") {
		t.Fatal("old password survived a force reset")
	}
	if s, err := st.SessionByTokenHash(ctxBg(), auth.HashToken(staleTok)); err != nil || s != nil {
		t.Fatal("pre-reset session survived the force reset")
	}
}

// pwOK checks a candidate password against the stored bcrypt hash.
func pwOK(t *testing.T, st *store.Store, username, pass string) bool {
	t.Helper()
	u, err := st.UserByUsername(ctxBg(), username)
	if err != nil || u == nil {
		t.Fatal("user lookup failed")
	}
	return bcryptCheck(u.PasswordHash, pass)
}

// bcryptCheck compares a plaintext password against a stored hash.
func bcryptCheck(hash, pass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
}
