package agent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
)

// systemCertPool is the OS trust-store source. A package-level seam so tests
// can substitute a controlled stand-in pool and prove the APPEND semantics
// (custom roots added, pre-existing public roots untouched) without needing
// network access to a real public CA.
var systemCertPool = x509.SystemCertPool

// loadAgentCAPool reads the PEM file at path and APPENDS its certificates to
// the system trust pool. Appending (never replacing) is the contract: public
// CAs keep working for any other HTTPS traffic — conditional on the system
// pool actually containing roots (a rootless/minimal container image may ship
// no CA bundle, leaving the pool custom-only). The custom CA only adds trust
// for the farm's own controller certificate.
//
// Non-CA anchors: Go accepts a leaf certificate as a trust root (de-facto
// pinning). That is legitimate but breaks at the next server-cert rotation
// and contradicts the "agents trust the CA" runbook, so warn — never reject.
//
// Fail-fast paths (the agent must not start with a broken trust config):
//   - unreadable/missing file → error naming ENCODE_AGENT_CA_CERT and the path
//   - file with no valid PEM certificate → error (a typo'd path that happens
//     to exist, an HTML error page saved as .crt, or an empty file)
func loadAgentCAPool(path string, log *slog.Logger) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ENCODE_AGENT_CA_CERT: cannot read %s: %w (point the variable at a readable PEM certificate file)", path, err)
	}
	pool, err := systemCertPool()
	if err != nil {
		return nil, fmt.Errorf("ENCODE_AGENT_CA_CERT: load system cert pool: %w", err)
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("ENCODE_AGENT_CA_CERT: %s contains no valid PEM certificates", path)
	}
	warnNonCAAnchors(pemBytes, path, log)
	return pool, nil
}

// warnNonCAAnchors inspects every appended PEM certificate and warns about
// any that is not a CA. A leaf anchor verifies today (Go pins the exact cert)
// but silently breaks the farm at the next certificate rotation — the runbook
// tells operators agents trust the CA, not the leaf. Warning only: pinning is
// a legitimate (if fragile) choice, so the agent still starts and works.
func warnNonCAAnchors(pemBytes []byte, path string, log *slog.Logger) {
	if log == nil {
		return
	}
	for rest := pemBytes; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue // AppendCertsFromPEM already rejected invalid blocks
		}
		if !cert.IsCA {
			log.Warn("ENCODE_AGENT_CA_CERT trust anchor is a leaf certificate — pinning breaks at rotation; use the CA cert",
				"path", path, "subject", cert.Subject.String())
		}
	}
}

// newControllerClient builds the HTTP client used for EVERY controller-facing
// request: pairing, heartbeats, job-completion reports (postJSON) and the
// lib/bin/agent update downloads (getAuth) all share this one client.
//
// caCertFile empty → the plain default client, byte-identical to the
// historical behavior (system roots, default transport tuning). Set → the
// custom CA is appended to the system pool and used as RootCAs. There is no
// InsecureSkipVerify anywhere: an untrusted or wrong-hostname controller
// certificate still fails the handshake, opt-in env or not.
func newControllerClient(caCertFile string, log *slog.Logger) (*http.Client, error) {
	// No hard client Timeout: large payload downloads are governed by
	// per-request contexts (heartbeat uses 30s, updates 30m). A fixed
	// client-wide cap would contradict the download windows.
	if caCertFile == "" {
		return &http.Client{}, nil
	}
	pool, err := loadAgentCAPool(caCertFile, log)
	if err != nil {
		return nil, err
	}
	// Clone the default transport so dial/proxy/keep-alive tuning is
	// preserved; only the trust roots (and the protocol floor) change.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	return &http.Client{Transport: tr}, nil
}
