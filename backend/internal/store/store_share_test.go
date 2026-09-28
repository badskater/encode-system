package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestShareCRUD covers create/list/get/update/delete of shares, password
// encryption at rest, and role lookup.
func TestShareCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sh, err := s.CreateShare(ctx, &model.Share{
		Name: "unraid-scripts", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "unraid01", Path: "encodes\\scripts", Username: "encode", Password: "s3cret",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sh.ID == 0 {
		t.Fatal("no id")
	}
	// Returned struct has the plaintext password (caller just supplied it).
	if sh.Password != "s3cret" {
		t.Fatalf("password = %q", sh.Password)
	}

	// At rest: the raw DB row must NOT contain the plaintext.
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT password FROM shares WHERE id=?", sh.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "s3cret" || raw == "" {
		t.Fatalf("password at rest = %q, want ciphertext", raw)
	}

	// Get decrypts.
	got, err := s.GetShare(ctx, sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "s3cret" || got.Kind != model.ShareSMB || got.Role != model.ShareRoleScripts {
		t.Fatalf("get = %+v", got)
	}

	// List (mask passwords at the API layer, not here — store returns
	// decrypted so provisioning can use it).
	all, err := s.ListShares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Password != "s3cret" {
		t.Fatalf("list = %+v", all)
	}

	// Role lookup: active share for scripts.
	byRole, err := s.ShareForRole(ctx, model.ShareRoleScripts)
	if err != nil || byRole == nil || byRole.ID != sh.ID {
		t.Fatalf("shareForRole = %+v err=%v", byRole, err)
	}

	// Update: password empty = keep existing.
	sh.Path = "encodes\\scripts2"
	sh.Password = ""
	if err := s.UpdateShare(ctx, sh); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetShare(ctx, sh.ID)
	if got.Path != "encodes\\scripts2" || got.Password != "s3cret" {
		t.Fatalf("after update = %+v", got)
	}

	// Disabled shares are skipped by ShareForRole.
	sh.Enabled = false
	if err := s.UpdateShare(ctx, sh); err != nil {
		t.Fatal(err)
	}
	byRole, err = s.ShareForRole(ctx, model.ShareRoleScripts)
	if err != nil || byRole != nil {
		t.Fatalf("disabled shareForRole = %+v err=%v", byRole, err)
	}

	// Delete.
	if err := s.DeleteShare(ctx, sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetShare(ctx, sh.ID); err == nil {
		t.Fatal("get after delete: want error")
	}
}

// TestShareMigrationFromSettings verifies first-boot migration: NFS server +
// share paths in Settings become nfs-kind shares; existing shares are never
// duplicated.
func TestShareMigrationFromSettings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	st := &model.Settings{
		NFSServer:    "unraid01",
		ScriptsShare: "/mnt/user/scripts",
		ReleaseShare: "/mnt/user/ReleaseFolders",
	}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if n := s.MigrateSettingsShares(ctx); n != 2 {
		t.Fatalf("migrated = %d, want 2", n)
	}
	all, _ := s.ListShares(ctx)
	if len(all) != 2 {
		t.Fatalf("shares = %d, want 2", len(all))
	}
	var kinds, roles []string
	for _, sh := range all {
		kinds = append(kinds, string(sh.Kind))
		roles = append(roles, string(sh.Role))
		if sh.Server != "unraid01" {
			t.Fatalf("server = %q", sh.Server)
		}
	}
	// Idempotent: second run adds nothing.
	if n := s.MigrateSettingsShares(ctx); n != 0 {
		t.Fatalf("re-migrated = %d, want 0", n)
	}
}
