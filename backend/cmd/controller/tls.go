package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"time"
)

// resolveTLSConfig builds the controller's server TLS configuration from the
// ENCODE_TLS_CERT / ENCODE_TLS_KEY values (PEM file paths).
//
//   - Neither set → (nil, nil): plain HTTP, byte-identical to the historical
//     default — existing deploys see zero behavior change.
//   - Exactly one set → an error naming the MISSING variable: a half-wired
//     TLS setup must fail fast at startup, never silently fall back to HTTP
//     (that would defeat the operator's intent to encrypt the fleet).
//   - Both set → load the keypair now (unreadable/invalid files fail fast
//     with the offending paths) and pin TLS 1.2 as the protocol floor.
//
// A loaded certificate that is expired or not-yet-valid is a WARNING, not a
// startup failure: the operator may be mid-rotation and the controller must
// still come up (every handshake will fail until the cert is replaced, so the
// warning names the path and NotAfter to make that diagnosable at boot).
//
// The returned config is attached to the ONE http.Server, so the API,
// /metrics, health and the SPA all move to TLS together — there is no
// second listener and no plaintext port left behind.
func resolveTLSConfig(certFile, keyFile string, log *slog.Logger) (*tls.Config, error) {
	switch {
	case certFile == "" && keyFile == "":
		return nil, nil
	case certFile == "":
		return nil, fmt.Errorf("ENCODE_TLS_KEY is set but ENCODE_TLS_CERT is missing: set both to PEM file paths (certificate + private key) or neither for plain HTTP")
	case keyFile == "":
		return nil, fmt.Errorf("ENCODE_TLS_CERT is set but ENCODE_TLS_KEY is missing: set both to PEM file paths (certificate + private key) or neither for plain HTTP")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair (ENCODE_TLS_CERT=%s ENCODE_TLS_KEY=%s): %w — both files must exist, be readable by the controller, and hold a matching PEM certificate and private key", certFile, keyFile, err)
	}
	warnCertValidity(cert, certFile, log)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// warnCertValidity logs a warning when the loaded leaf certificate is outside
// its validity window. It never fails startup (an operator mid-rotation may
// boot with an about-to-be-replaced cert); it just surfaces the problem at
// boot instead of letting every fleet handshake fail with a bare x509 error.
func warnCertValidity(cert tls.Certificate, certFile string, log *slog.Logger) {
	if log == nil {
		return
	}
	leaf := cert.Leaf
	if leaf == nil && len(cert.Certificate) > 0 {
		// LoadX509KeyPair does not always populate Leaf; parse the first
		// (leaf) DER block ourselves so the check works on every Go version.
		if parsed, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			leaf = parsed
		}
	}
	if leaf == nil {
		return
	}
	now := time.Now()
	switch {
	case now.After(leaf.NotAfter):
		log.Warn("TLS certificate is EXPIRED — every agent handshake will fail until it is replaced",
			"cert", certFile, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))
	case now.Before(leaf.NotBefore):
		log.Warn("TLS certificate is NOT YET VALID — every agent handshake will fail until NotBefore",
			"cert", certFile, "not_before", leaf.NotBefore.UTC().Format(time.RFC3339), "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
}
