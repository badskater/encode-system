// Package oidc implements optional OpenID Connect login for the management
// plane. It is OFF by default: with none of the ENCODE_OIDC_* core variables
// set, Resolve returns nil and the controller registers no OIDC routes,
// exposes no SSO button, and behaves byte-identically to a local-auth-only
// install. Local username/password login is never modified — both methods
// coexist when OIDC is enabled.
package oidc

import (
	"fmt"
	"net/url"
	"strings"
)

// Config holds the validated OIDC settings resolved from the environment.
// A nil *Config anywhere in the controller means "OIDC disabled".
type Config struct {
	Issuer       string // ENCODE_OIDC_ISSUER — provider identifier (must match the discovery "iss" exactly)
	ClientID     string // ENCODE_OIDC_CLIENT_ID
	ClientSecret string // ENCODE_OIDC_CLIENT_SECRET — never logged
	RedirectURL  string // ENCODE_OIDC_REDIRECT_URL — must match the provider's registered redirect URI
	// AdminGroup is optional (ENCODE_OIDC_ADMIN_GROUP). When set, a login
	// whose groups claim contains this value gets the admin role; everyone
	// else gets viewer. When empty, NEW accounts are provisioned as viewer
	// and EXISTING accounts keep their stored role (no group mapping is
	// configured, so there is nothing to sync from).
	AdminGroup string
	// GroupsClaim names the ID-token claim carrying the user's groups
	// (ENCODE_OIDC_GROUPS_CLAIM, default "groups").
	GroupsClaim string
}

// coreVarNames lists the four variables that must ALL be set to enable OIDC,
// in error-message order.
var coreVarNames = []string{
	"ENCODE_OIDC_ISSUER",
	"ENCODE_OIDC_CLIENT_ID",
	"ENCODE_OIDC_CLIENT_SECRET",
	"ENCODE_OIDC_REDIRECT_URL",
}

// Resolve validates the raw environment values and builds a Config.
//
//   - All four core values empty → (nil, nil): OIDC fully disabled, zero
//     behavior change (the historical default).
//   - Some set, some missing → an error naming BOTH the set and the missing
//     variables: a half-wired OIDC setup fails fast at startup, never
//     silently falls back to local-only (same pattern as resolveTLSConfig).
//   - All four set → URL sanity checks on issuer and redirect URL, then a
//     Config with GroupsClaim defaulted to "groups".
//
// Optional values (adminGroup, groupsClaim) never enable or disable OIDC on
// their own — they only take effect when the four core variables are set.
func Resolve(issuer, clientID, clientSecret, redirectURL, adminGroup, groupsClaim string) (*Config, error) {
	values := []string{issuer, clientID, clientSecret, redirectURL}
	var set, missing []string
	for i, v := range values {
		if v == "" {
			missing = append(missing, coreVarNames[i])
		} else {
			set = append(set, coreVarNames[i])
		}
	}
	if len(set) == 0 {
		return nil, nil // disabled — the default
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s set but %s missing: set all four core OIDC variables (%s) or none to keep OIDC login disabled",
			strings.Join(set, ", "), strings.Join(missing, ", "), strings.Join(coreVarNames, ", "))
	}
	for _, u := range []struct{ name, val string }{
		{"ENCODE_OIDC_ISSUER", issuer},
		{"ENCODE_OIDC_REDIRECT_URL", redirectURL},
	} {
		parsed, err := url.Parse(u.val)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("%s must be an absolute http(s) URL, got %q", u.name, u.val)
		}
	}
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	return &Config{
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AdminGroup:   adminGroup,
		GroupsClaim:  groupsClaim,
	}, nil
}
