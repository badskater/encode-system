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

// ---------- Truncation collision safety (quality review Q1) ----------

// Two DISTINCT IdP subjects whose preferred_username truncates to the same
// 64-char prefix must NEVER share one local account: the over-long name gets
// a collision-resistant suffix derived from the raw sub.
func TestUsernameTruncationCollisionResistant(t *testing.T) {
	prefix := strings.Repeat("a", 70)
	u1, err := Username(prefix+"-victim", "", "sub-victim")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := Username(prefix+"-attacker", "", "sub-attacker")
	if err != nil {
		t.Fatal(err)
	}
	if u1 == u2 {
		t.Fatalf("distinct subjects merged into one account: %q", u1)
	}
	for _, u := range []string{u1, u2} {
		if len(u) > maxUsernameLen {
			t.Fatalf("username exceeds cap: %q (%d)", u, len(u))
		}
		// Shape: 56-char prefix + '-' + 7 lowercase hex chars.
		if len(u) != collisionPrefixLen+1+collisionHashLen {
			t.Fatalf("unexpected suffix shape: %q (%d)", u, len(u))
		}
		suffix := u[len(u)-collisionHashLen:]
		for _, r := range suffix {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("suffix not lowercase hex: %q", u)
			}
		}
		if u[collisionPrefixLen] != '-' {
			t.Fatalf("missing separator before hash: %q", u)
		}
		if !strings.HasPrefix(u, strings.Repeat("a", collisionPrefixLen)) {
			t.Fatalf("prefix lost: %q", u)
		}
	}
	// Deterministic: same identity → same name on every login.
	again, _ := Username(prefix+"-victim", "", "sub-victim")
	if again != u1 {
		t.Fatalf("collision-resistant derivation not deterministic: %q vs %q", again, u1)
	}
}

// Names at or under the cap keep their exact shape — no suffix is appended.
func TestUsernameShortNamesUnchanged(t *testing.T) {
	exact := strings.Repeat("b", maxUsernameLen)
	u, err := Username(exact, "", "sub")
	if err != nil || u != exact {
		t.Fatalf("64-char name altered: %q err=%v", u, err)
	}
	u, err = Username("alice", "", "sub")
	if err != nil || u != "alice" {
		t.Fatalf("short name altered: %q err=%v", u, err)
	}
}

// The suffix keys on the RAW sub, so it also separates long subjects used
// directly as the username fallback.
func TestUsernameLongSubjectCollisionResistant(t *testing.T) {
	prefix := strings.Repeat("z", 80)
	u1, err := Username("", "", prefix+"|one")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := Username("", "", prefix+"|two")
	if err != nil {
		t.Fatal(err)
	}
	if u1 == u2 || len(u1) > maxUsernameLen || len(u2) > maxUsernameLen {
		t.Fatalf("long subjects collided: %q vs %q", u1, u2)
	}
}

// ---------- ASCII-first sanitization (quality review Q2) ----------

// Unicode case folding must never manufacture ASCII letters: U+0130 (İ)
// folds to 'i' and U+212A (K) folds to 'k' under strings.ToLower, which let
// 'admİn' land on the local 'admin' account. Non-ASCII runes become
// separators BEFORE any lowercasing.
func TestSanitizeConfusablesNeverASCII(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"admİn", "adm-n"},      // U+0130 must NOT become 'i'
		{"Admİn", "adm-n"},      // mixed case + confusable
		{"adm\u212An", "adm-n"}, // Kelvin sign must NOT become 'k'
		{"\u212A", ""},          // lone Kelvin sign → empty
		{"İ", ""},               // lone dotted capital I → empty
		{"ａｄｍｉｎ", ""},           // fullwidth stays non-ASCII (sub fallback)
		{"Ａdmin", "dmin"},       // fullwidth A → separator, trimmed
		{"Alice", "alice"},      // plain ASCII lowercasing still works
		{"ADMIN", "admin"},      // ASCII uppercase still folds
	} {
		if got := sanitizeUsername(tc.in); got != tc.want {
			t.Errorf("sanitizeUsername(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// End-to-end: the confusable derives a NON-admin username.
	u, err := Username("admİn", "", "sub-conf")
	if err != nil || u != "adm-n" {
		t.Fatalf("Username(admİn) = %q err=%v, want adm-n", u, err)
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
