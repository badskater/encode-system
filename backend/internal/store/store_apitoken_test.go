package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestAPITokenCRUD covers create/list/get-by-hash/delete of API tokens.
func TestAPITokenCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok := &model.APIToken{Name: "sonarr", Scope: model.TokenScopeAdmin, TokenHash: "hash-1"}
	if err := s.CreateAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if tok.ID == 0 {
		t.Fatal("no id assigned")
	}

	// Duplicate name rejected.
	dup := &model.APIToken{Name: "sonarr", Scope: model.TokenScopeRead, TokenHash: "hash-2"}
	if err := s.CreateAPIToken(ctx, dup); err == nil {
		t.Fatal("duplicate name accepted")
	}

	got, err := s.APITokenByHash(ctx, "hash-1")
	if err != nil || got == nil {
		t.Fatalf("by hash: %v %v", got, err)
	}
	if got.Name != "sonarr" || got.Scope != model.TokenScopeAdmin {
		t.Fatalf("wrong token: %+v", got)
	}

	// Unknown hash: nil, nil.
	missing, err := s.APITokenByHash(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("missing hash: %v %v", missing, err)
	}

	list, err := s.ListAPITokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d, want 1", len(list))
	}

	if err := s.DeleteAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ListAPITokens(ctx)
	if len(after) != 0 {
		t.Fatalf("after delete = %d rows", len(after))
	}
	// Delete unknown id is not an error (idempotent).
	if err := s.DeleteAPIToken(ctx, 999); err != nil {
		t.Fatal(err)
	}
}

// TestAPITokenTouch verifies last_used_at updates without changing other fields.
func TestAPITokenTouch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok := &model.APIToken{Name: "dash", Scope: model.TokenScopeRead, TokenHash: "h"}
	if err := s.CreateAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.APITokenByHash(ctx, "h")
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at not set")
	}
	if got.Name != "dash" || got.Scope != model.TokenScopeRead {
		t.Fatalf("touch mutated other fields: %+v", got)
	}
}
