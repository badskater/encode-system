package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
)

// ErrorSummary reduces a LoginURL/Callback error to a SANITIZED class for
// server logs: error kind and HTTP status only. Raw provider response bodies
// must never reach the logs (or the client) — a token-endpoint error body can
// echo authorization codes, client secrets or PII back at the caller.
// Known controller-side messages are fixed strings and safe verbatim;
// anything unrecognized collapses to a generic class.
func ErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	// oauth2.RetrieveError.Error() embeds the raw IdP response body —
	// extract only the HTTP status.
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.Response != nil {
			return fmt.Sprintf("token exchange rejected (HTTP %d)", re.Response.StatusCode)
		}
		return "token exchange rejected"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "provider request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	// Network errors may carry URL/response detail — keep the class only.
	var ue *url.Error
	if errors.As(err, &ue) {
		return "provider network error"
	}
	msg := err.Error()
	// Wrapped errors whose inner detail may contain provider output:
	// log the stage, never the detail.
	switch {
	case strings.HasPrefix(msg, "OIDC discovery"):
		return "provider discovery failed"
	case strings.HasPrefix(msg, "token exchange"):
		return "token exchange failed"
	case strings.HasPrefix(msg, "id token verification"):
		return "id token verification failed"
	case strings.HasPrefix(msg, "id token claims"):
		return "id token claims unreadable"
	}
	// Fixed controller-side messages carry no provider data.
	for _, safe := range []string{
		"missing code or state",
		"unknown or expired login state",
		"login flow cookie missing or mismatched",
		"token response carried no id_token",
		"id token nonce mismatch",
		"id token has no sub claim",
		"too many pending OIDC logins",
	} {
		if strings.HasPrefix(msg, safe) {
			return msg
		}
	}
	return "login rejected"
}
