package store

import (
	"context"
	"testing"
)

// TestNodePlatformMigrationDefault verifies the platform column defaults to
// "windows" for nodes created by a freshly migrated schema. This is the
// backward-compat invariant: legacy rows and old agents report "windows"
// unless a heartbeat says otherwise.
func TestNodePlatformMigrationDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, err := s.CreateNode(ctx, "enc-01", "hash-a")
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if n.Platform != "windows" {
		t.Fatalf("new node platform = %q, want %q (migration default)", n.Platform, "windows")
	}

	// Re-read via every lookup path to confirm the default is persisted,
	// not just a struct zero value echoed back.
	got, err := s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Platform != "windows" {
		t.Fatalf("GetNode platform = %q, want %q", got.Platform, "windows")
	}

	byName, err := s.NodeByName(ctx, n.Name)
	if err != nil {
		t.Fatalf("node by name: %v", err)
	}
	if byName.Platform != "windows" {
		t.Fatalf("NodeByName platform = %q, want %q", byName.Platform, "windows")
	}

	byToken, err := s.NodeByTokenHash(ctx, n.TokenHash)
	if err != nil {
		t.Fatalf("node by token: %v", err)
	}
	if byToken.Platform != "windows" {
		t.Fatalf("NodeByTokenHash platform = %q, want %q", byToken.Platform, "windows")
	}

	list, err := s.ListNodes(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list nodes: got %d, err %v", len(list), err)
	}
	if list[0].Platform != "windows" {
		t.Fatalf("ListNodes platform = %q, want %q", list[0].Platform, "windows")
	}
}

// TestNodePlatformRoundTrip verifies UpdateNode persists the platform field and
// it survives a re-read. A linux node must round-trip as "linux".
func TestNodePlatformRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, err := s.CreateNode(ctx, "enc-01", "hash-a")
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	n.Platform = "linux"
	if err := s.UpdateNode(ctx, n); err != nil {
		t.Fatalf("update node: %v", err)
	}
	got, err := s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Platform != "linux" {
		t.Fatalf("platform not persisted: got %q, want %q", got.Platform, "linux")
	}
}
