package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// ErrorSummary must never carry raw provider response bodies — they can echo
// authorization codes, client secrets or PII (quality review Q3).
func TestErrorSummaryNeverCarriesProviderBody(t *testing.T) {
	const marker = "SECRET-BODY-MARKER-9f8e7d"
	body := `{"error":"invalid_grant","error_description":"` + marker + `"}`
	re := &oauth2.RetrieveError{
		Response: &http.Response{StatusCode: http.StatusBadRequest},
		Body:     []byte(body),
	}
	for _, err := range []error{
		re,
		fmt.Errorf("token exchange: %w", re), // the Callback wrap
	} {
		got := ErrorSummary(err)
		if strings.Contains(got, marker) {
			t.Fatalf("summary leaked the provider body: %q", got)
		}
		if !strings.Contains(got, "400") {
			t.Fatalf("summary lost the HTTP status: %q", got)
		}
	}
}

func TestErrorSummaryClasses(t *testing.T) {
	const marker = "SECRET-BODY-MARKER-9f8e7d"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline", fmt.Errorf("token exchange: %w", context.DeadlineExceeded), "provider request timed out"},
		{"canceled", context.Canceled, "request canceled"},
		{"network", &url.Error{Op: "Post", URL: "https://idp.example/token", Err: errors.New("dial failed")}, "provider network error"},
		{"discovery-with-body", fmt.Errorf("OIDC discovery for https://idp.example: %s", marker), "provider discovery failed"},
		{"verification-detail", fmt.Errorf("id token verification: oidc: malformed token %s", marker), "id token verification failed"},
		{"claims-detail", fmt.Errorf("id token claims: %s", marker), "id token claims unreadable"},
		{"exchange-generic", errors.New("token exchange: something odd " + marker), "token exchange failed"},
		{"fixed-state", errors.New("unknown or expired login state (start the login again)"), "unknown or expired login state (start the login again)"},
		{"fixed-cookie", errors.New("login flow cookie missing or mismatched (start the login again)"), "login flow cookie missing or mismatched (start the login again)"},
		{"fixed-nonce", errors.New("id token nonce mismatch"), "id token nonce mismatch"},
		{"unknown", errors.New("something entirely different " + marker), "login rejected"},
	} {
		got := ErrorSummary(tc.err)
		if got != tc.want {
			t.Errorf("%s: ErrorSummary = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, marker) {
			t.Errorf("%s: summary leaked the marker: %q", tc.name, got)
		}
	}
}
