package oidc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// maxUsernameLen caps derived usernames. The local user API has no length
// limit, but OIDC usernames come from an external identity provider and must
// stay bounded and display-safe.
const maxUsernameLen = 64

// Over-long names keep a collisionPrefixLen-char prefix plus '-' and a
// collisionHashLen-char hex digest of the raw sub (56 + 1 + 7 = 64), so two
// distinct subjects truncating to the same prefix never share one account.
const (
	collisionPrefixLen = 56
	collisionHashLen   = 7
)

// Identity is the subset of ID-token claims the controller consumes.
type Identity struct {
	Subject           string // "sub" — stable provider-side user id
	PreferredUsername string // "preferred_username"
	Email             string // "email"
	Groups            []string
}

// Username derives the local account name from an OIDC identity:
// preferred_username first, then the email local part, then the subject.
// Each candidate is sanitized (see sanitizeUsername); the first candidate
// that yields a non-empty result wins. The derivation is deterministic, so
// repeated logins of the same identity always land on the same local account
// (auto-provisioning is idempotent). An identity with no usable candidate is
// rejected — the controller never invents a random username.
//
// Over-long names are made collision-resistant instead of plain-truncated:
// a 56-char prefix plus a 7-char hex digest of the RAW sub. Plain truncation
// merged distinct IdP subjects sharing a 64-char prefix into ONE local
// account (role sync then let one identity flip the other's role).
func Username(preferredUsername, email, subject string) (string, error) {
	candidates := []string{preferredUsername, emailLocalPart(email), subject}
	for _, c := range candidates {
		u := sanitizeUsername(c)
		if u == "" {
			continue
		}
		if len(u) <= maxUsernameLen {
			return u, nil
		}
		return collisionResistantName(u, c, subject), nil
	}
	return "", errors.New("OIDC identity has no usable username (preferred_username, email and sub are all empty or invalid)")
}

// collisionResistantName shortens an over-long sanitized name to a fixed
// prefix plus '-' and a short SHA-256 digest of the raw subject. The digest
// keys on the provider-side identity (sub), so distinct subjects always get
// distinct accounts even when their names share the whole prefix; the same
// identity keeps the same name on every login. When the provider sent no
// sub, the raw candidate is hashed instead (still deterministic).
func collisionResistantName(sanitized, rawCandidate, subject string) string {
	// sanitized is ASCII-only, so byte slicing is rune-safe.
	prefix := strings.Trim(sanitized[:collisionPrefixLen], "-._")
	hashSrc := subject
	if hashSrc == "" {
		hashSrc = rawCandidate
	}
	sum := sha256.Sum256([]byte(hashSrc))
	return prefix + "-" + hex.EncodeToString(sum[:])[:collisionHashLen]
}

// emailLocalPart returns the part before the last '@' (the domain is never
// part of the username). An email without '@' is returned unchanged.
func emailLocalPart(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return email[:i]
	}
	return email
}

// sanitizeUsername normalizes an external candidate to the app's username
// shape: restricted to [a-z0-9._-], runs of separators collapsed to a single
// '-', leading/trailing separators trimmed. Lowercasing is ASCII-ONLY and
// happens per rune AFTER the non-ASCII decision: every non-ASCII rune becomes
// a separator, so Unicode case folding can never manufacture ASCII letters
// (strings.ToLower maps U+0130 İ→'i' and U+212A K→'k', which let 'admİn'
// land on the local 'admin' account). The length cap lives in Username,
// which can apply the collision-resistant suffix. Local accounts are matched
// exactly, so "Alice" and "alice" are deliberately distinct accounts —
// documented in Docs/md/Deployment.md.
func sanitizeUsername(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevSep = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_':
			b.WriteRune(r)
			prevSep = false
		default:
			// Every other rune (spaces, '@', '|', ALL non-ASCII including
			// confusables, control chars) becomes a single collapsed '-'.
			if !prevSep {
				b.WriteRune('-')
				prevSep = true
			}
		}
	}
	return strings.Trim(b.String(), "-._")
}

// RoleFor maps the identity's groups to a local role: admin when AdminGroup
// is configured AND present in the groups claim, viewer otherwise. With no
// AdminGroup configured every identity maps to viewer (existing accounts
// keep their stored role — the caller only syncs when AdminGroup is set).
func (c *Config) RoleFor(groups []string) string {
	if c.AdminGroup != "" {
		for _, g := range groups {
			if g == c.AdminGroup {
				return model.RoleAdmin
			}
		}
	}
	return model.RoleViewer
}

// groupsFromClaim extracts a group list from a raw claim value. Providers
// disagree on shape: most emit []string, some a single string, and JSON
// round-trips produce []any. Anything else yields no groups (fail closed to
// viewer, never to admin).
func groupsFromClaim(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
