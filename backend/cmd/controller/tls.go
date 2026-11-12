package main

import (
	"crypto/tls"
	"fmt"
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
// The returned config is attached to the ONE http.Server, so the API,
// /metrics, health and the SPA all move to TLS together — there is no
// second listener and no plaintext port left behind.
func resolveTLSConfig(certFile, keyFile string) (*tls.Config, error) {
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
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
