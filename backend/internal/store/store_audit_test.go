package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestAuditAppendList verifies audit rows persist and list newest-first
// with the limit applied.
func TestAuditAppendList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := s.AppendAudit(ctx, "admin", "settings.update", "settings", `{"i":`+string(rune('0'+i))+`}`); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAudit(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("list = %d rows, want 3 (limit)", len(got))
	}
	// Newest first: ids descend.
	if got[0].ID <= got[1].ID || got[1].ID <= got[2].ID {
		t.Fatalf("not newest-first: %d %d %d", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Actor != "admin" || got[0].Action != "settings.update" {
		t.Fatalf("row = %+v", got[0])
	}
}

// TestAuditListEmpty verifies an empty table returns an empty slice, not nil
// (JSON renders [] not null).
func TestAuditListEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %v", got)
	}
	_ = model.AuditEvent{} // keep model import honest
}
