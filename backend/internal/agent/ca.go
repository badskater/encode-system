package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
// CAs keep working for any other HTTPS traffic, and the custom CA only adds
// trust for the farm's own controller certificate.
//
// Fail-fast paths (the agent must not start with a broken trust config):
//   - unreadable/missing file → error naming ENCODE_AGENT_CA_CERT and the path
//   - file with no valid PEM certificate → error (a typo'd path that happens
//     to exist, an HTML error page saved as .crt, or an empty file)
func loadAgentCAPool(path string) (*x509.CertPool, error) {
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
	return pool, nil
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
func newControllerClient(caCertFile string) (*http.Client, error) {
	// No hard client Timeout: large payload downloads are governed by
	// per-request contexts (heartbeat uses 30s, updates 30m). A fixed
	// client-wide cap would contradict the download windows.
	if caCertFile == "" {
		return &http.Client{}, nil
	}
	pool, err := loadAgentCAPool(caCertFile)
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
