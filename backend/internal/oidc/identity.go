package oidc

import (
	"errors"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// maxUsernameLen caps derived usernames. The local user API has no length
// limit, but OIDC usernames come from an external identity provider and must
// stay bounded and display-safe.
const maxUsernameLen = 64

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
func Username(preferredUsername, email, subject string) (string, error) {
	candidates := []string{preferredUsername, emailLocalPart(email), subject}
	for _, c := range candidates {
		if u := sanitizeUsername(c); u != "" {
			return u, nil
		}
	}
	return "", errors.New("OIDC identity has no usable username (preferred_username, email and sub are all empty or invalid)")
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
// shape: lowercase, restricted to [a-z0-9._-], runs of separators collapsed
// to a single '-', leading/trailing separators trimmed, capped at
// maxUsernameLen. Lowercasing keeps matching stable across providers that
// differ in case (local accounts are matched exactly, so "Alice" and "alice"
// are deliberately distinct accounts — documented in Docs/md/Deployment.md).
func sanitizeUsername(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_':
			b.WriteRune(r)
			prevSep = false
		default:
			// Every other rune (spaces, '@', '|', unicode, control chars)
			// becomes a single collapsed '-'.
			if !prevSep {
				b.WriteRune('-')
				prevSep = true
			}
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > maxUsernameLen {
		out = strings.Trim(out[:maxUsernameLen], "-._")
	}
	return out
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
