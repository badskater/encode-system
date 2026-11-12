package oidc

import (
	"strings"
	"testing"
)

// ---------- Resolve: env gating + fail-fast ----------

func TestResolveAllUnsetDisables(t *testing.T) {
	cfg, err := Resolve("", "", "", "", "", "")
	if err != nil {
		t.Fatalf("all-unset must not error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("all-unset must disable OIDC, got %+v", cfg)
	}
}

func TestResolveOptionalOnlyStillDisabled(t *testing.T) {
	// Admin group / groups claim alone must NOT enable OIDC.
	cfg, err := Resolve("", "", "", "", "admins", "roles")
	if err != nil || cfg != nil {
		t.Fatalf("optional-only must stay disabled: cfg=%+v err=%v", cfg, err)
	}
}

func TestResolvePartialFailsFast(t *testing.T) {
	full := []string{"https://idp.example", "client-1", "secret-x", "https://ctl.example/api/auth/oidc/callback"}
	for missing := 0; missing < 4; missing++ {
		vals := append([]string{}, full...)
		vals[missing] = ""
		_, err := Resolve(vals[0], vals[1], vals[2], vals[3], "", "")
		if err == nil {
			t.Fatalf("missing %s must fail fast", coreVarNames[missing])
		}
		// The error must name the missing variable AND the disabled fallback.
		if !strings.Contains(err.Error(), coreVarNames[missing]) {
			t.Fatalf("error does not name missing var %s: %v", coreVarNames[missing], err)
		}
		if !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("error does not explain the all-unset fallback: %v", err)
		}
	}
	// Only one set — still partial, still fails.
	if _, err := Resolve("https://idp.example", "", "", "", "", ""); err == nil {
		t.Fatal("issuer-only must fail fast")
	}
}

func TestResolveFullConfig(t *testing.T) {
	cfg, err := Resolve("https://idp.example", "client-1", "secret-x", "https://ctl.example/api/auth/oidc/callback", "encode-admins", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer != "https://idp.example" || cfg.ClientID != "client-1" || cfg.ClientSecret != "secret-x" {
		t.Fatalf("core values lost: %+v", cfg)
	}
	if cfg.RedirectURL != "https://ctl.example/api/auth/oidc/callback" || cfg.AdminGroup != "encode-admins" {
		t.Fatalf("optional values lost: %+v", cfg)
	}
	if cfg.GroupsClaim != "groups" {
		t.Fatalf("groups claim default = %q, want groups", cfg.GroupsClaim)
	}
	// Explicit claim name wins.
	cfg, err = Resolve("https://idp.example", "client-1", "secret-x", "https://ctl.example/cb", "", "roles")
	if err != nil || cfg.GroupsClaim != "roles" {
		t.Fatalf("custom groups claim: %+v err=%v", cfg, err)
	}
}

func TestResolveRejectsNonURLs(t *testing.T) {
	for _, tc := range []struct{ name, issuer, redirect string }{
		{"issuer-not-url", "not a url", "https://ctl.example/cb"},
		{"issuer-relative", "/relative", "https://ctl.example/cb"},
		{"issuer-bad-scheme", "ftp://idp.example", "https://ctl.example/cb"},
		{"redirect-not-url", "https://idp.example", "::::"},
		{"redirect-relative", "https://idp.example", "/api/auth/oidc/callback"},
	} {
		if _, err := Resolve(tc.issuer, "c", "s", tc.redirect, "", ""); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}
