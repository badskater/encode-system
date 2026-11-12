package oidc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Username derivation ----------

func TestUsernamePrefersPreferredUsername(t *testing.T) {
	u, err := Username("Alice", "alice@example.org", "sub-123")
	if err != nil || u != "alice" {
		t.Fatalf("got %q err=%v, want alice", u, err)
	}
}

func TestUsernameFallsBackToEmailLocalPart(t *testing.T) {
	u, err := Username("", "Bob.Builder@example.org", "sub-123")
	if err != nil || u != "bob.builder" {
		t.Fatalf("got %q err=%v, want bob.builder", u, err)
	}
}

func TestUsernameFallsBackToSubject(t *testing.T) {
	u, err := Username("", "", "auth0|abc123")
	if err != nil || u != "auth0-abc123" {
		t.Fatalf("got %q err=%v, want auth0-abc123", u, err)
	}
}

func TestUsernameSkipsUnusableCandidates(t *testing.T) {
	// preferred_username sanitizes to empty → email wins.
	u, err := Username("!!!", "carol@example.org", "sub-1")
	if err != nil || u != "carol" {
		t.Fatalf("got %q err=%v, want carol", u, err)
	}
	// Email without a usable local part → sub wins.
	u, err = Username("", "@example.org", "sub-2")
	if err != nil || u != "sub-2" {
		t.Fatalf("got %q err=%v, want sub-2", u, err)
	}
}

func TestUsernameSanitizes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  MiXeD Case  ", "mixed-case"},
		{"user..name", "user..name"}, // dots are legal, kept
		{"a---b", "a-b"},             // separator runs collapse
		{"-leading", "leading"},
		{"trailing._-", "trailing"},
		{"héllo wörld", "h-llo-w-rld"},     // non-ASCII → collapsed '-'
		{"a\tb\nc", "a-b-c"},               // control chars never survive
		{"../../etc/passwd", "etc-passwd"}, // leading separators trimmed away
	} {
		if got := sanitizeUsername(tc.in); got != tc.want {
			t.Errorf("sanitizeUsername(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUsernameLengthCap(t *testing.T) {
	u, err := Username(strings.Repeat("x", 300), "", "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(u) != maxUsernameLen {
		t.Fatalf("len = %d, want %d", len(u), maxUsernameLen)
	}
	// Truncation must not leave a trailing separator.
	u, err = Username(strings.Repeat("a", 63)+"---", "", "sub")
	if err != nil || strings.HasSuffix(u, "-") {
		t.Fatalf("truncation left separator: %q err=%v", u, err)
	}
}

func TestUsernameNoUsableCandidate(t *testing.T) {
	if _, err := Username("", "", ""); err == nil {
		t.Fatal("empty identity must be rejected")
	}
	if _, err := Username("!!!", "@", "???"); err == nil {
		t.Fatal("all-invalid identity must be rejected")
	}
}

func TestUsernameDeterministic(t *testing.T) {
	// Idempotent provisioning relies on identical derivation every login.
	a, _ := Username("Alice Smith", "alice@example.org", "sub-9")
	b, _ := Username("Alice Smith", "alice@example.org", "sub-9")
	if a != b || a != "alice-smith" {
		t.Fatalf("derivation not deterministic: %q vs %q", a, b)
	}
}

// ---------- Role mapping ----------

func TestRoleForAdminGroup(t *testing.T) {
	cfg := &Config{AdminGroup: "encode-admins"}
	if got := cfg.RoleFor([]string{"users", "encode-admins"}); got != model.RoleAdmin {
		t.Fatalf("admin group present → %q, want admin", got)
	}
	if got := cfg.RoleFor([]string{"users"}); got != model.RoleViewer {
		t.Fatalf("admin group absent → %q, want viewer", got)
	}
	if got := cfg.RoleFor(nil); got != model.RoleViewer {
		t.Fatalf("no groups → %q, want viewer", got)
	}
	// Substring must NOT match — exact group membership only.
	if got := cfg.RoleFor([]string{"encode-admins-read-only"}); got != model.RoleViewer {
		t.Fatalf("substring group matched: %q", got)
	}
}

func TestRoleForNoAdminGroupConfigured(t *testing.T) {
	cfg := &Config{}
	if got := cfg.RoleFor([]string{"anything"}); got != model.RoleViewer {
		t.Fatalf("no admin group configured → %q, want viewer", got)
	}
}

// ---------- Groups claim shapes ----------

func TestGroupsFromClaimShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want []string
	}{
		{"string-slice", []string{"a", "b"}, []string{"a", "b"}},
		{"any-slice", []any{"a", "b"}, []string{"a", "b"}},
		{"mixed-slice-drops-nonstrings", []any{"a", 42, nil, "b"}, []string{"a", "b"}},
		{"single-string", "admins", []string{"admins"}},
		{"empty-string", "", nil},
		{"absent", nil, nil},
		{"number-fails-closed", 42, nil},
		{"map-fails-closed", map[string]any{"x": "y"}, nil},
	} {
		if got := groupsFromClaim(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
}
