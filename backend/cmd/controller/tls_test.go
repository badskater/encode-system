package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/api"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// testPKI is a throwaway certificate authority + server keypair generated at
// RUNTIME (never committed, never a PEM literal in source). ECDSA P-256 keeps
// generation fast. The server cert carries the SANs a real controller cert
// needs: the localhost name plus the loopback IPs httptest/clients dial.
type testPKI struct {
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	serverDER []byte
	serverKey *ecdsa.PrivateKey
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	return newTestPKIWithServerValidity(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

// newTestPKIWithServerValidity is newTestPKI with a custom validity window on
// the SERVER cert (the CA stays valid) — for boot-time expiry-warning tests.
func newTestPKIWithServerValidity(t *testing.T, srvNotBefore, srvNotAfter time.Time) *testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "encode-test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "encode-controller"},
		NotBefore:    srvNotBefore,
		NotAfter:     srvNotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{caCert: caCert, caKey: caKey, serverDER: srvDER, serverKey: srvKey}
}

// caPEM returns the CA certificate in PEM form (what ENCODE_AGENT_CA_CERT
// points at on a real node).
func (p *testPKI) caPEM(t *testing.T) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw})
}

// writeServerPEM writes the server cert + key as PEM files and returns their
// paths — the ENCODE_TLS_CERT / ENCODE_TLS_KEY inputs.
func (p *testPKI) writeServerPEM(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "controller.crt")
	keyPath = filepath.Join(dir, "controller.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.serverDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(p.serverKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// pool trusts ONLY the test CA (client side).
func (p *testPKI) pool(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(p.caPEM(t)) {
		t.Fatal("test CA PEM did not append")
	}
	return pool
}

// buildTestMux wires the REAL controller API (store + update store + routes)
// exactly like main does for /api/ and /metrics, so the TLS tests exercise
// the actual handler surface, not a stub.
func buildTestMux(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "encode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up, err := update.NewStore(filepath.Join(dir, "updates"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := api.New(st, up, log, api.Config{
		AdminUsername: "admin",
		AdminPassword: strings.Repeat("pw", 8),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", srv.Routes())
	mux.Handle("/api/", srv.Routes())
	return mux
}

// TestResolveTLSConfigNeitherSetKeepsPlainHTTPDefault pins the zero-change
// contract: no env → nil config, and the same mux serves plain HTTP exactly
// as before (httptest regression over the real API).
func TestResolveTLSConfigNeitherSetKeepsPlainHTTPDefault(t *testing.T) {
	cfg, err := resolveTLSConfig("", "", discardLog())
	if err != nil || cfg != nil {
		t.Fatalf("neither set: want (nil, nil), got (%v, %v)", cfg, err)
	}
	ts := httptest.NewServer(buildTestMux(t))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plain-HTTP /api/health status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("health body = %q, want status ok", body)
	}
}

// TestResolveTLSConfigPartialFailsFast covers BOTH half-configured
// directions: the error must name the MISSING variable so the operator can
// fix .env without guessing.
func TestResolveTLSConfigPartialFailsFast(t *testing.T) {
	pki := newTestPKI(t)
	certPath, keyPath := pki.writeServerPEM(t)
	cases := []struct {
		name    string
		cert    string
		key     string
		missing string
	}{
		{"cert_only", certPath, "", "ENCODE_TLS_KEY"},
		{"key_only", "", keyPath, "ENCODE_TLS_CERT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := resolveTLSConfig(c.cert, c.key, discardLog())
			if err == nil {
				t.Fatalf("partial config must fail fast, got config %v", cfg)
			}
			if cfg != nil {
				t.Fatal("partial config must not return a usable TLS config")
			}
			if !strings.Contains(err.Error(), c.missing) {
				t.Fatalf("error %q must name the missing var %s", err, c.missing)
			}
		})
	}
}

// TestResolveTLSConfigLoadErrors covers the cert-load fail-fast paths:
// missing files, garbage PEM, and a cert/key pair that does not match. Every
// error must carry the offending path(s) so a bad mount is diagnosable.
func TestResolveTLSConfigLoadErrors(t *testing.T) {
	pki := newTestPKI(t)
	certPath, keyPath := pki.writeServerPEM(t)
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("this is not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT keypair: valid PEM on both sides, but the key does not
	// match the certificate — LoadX509KeyPair must reject it.
	other := newTestPKI(t)
	_, otherKeyPath := other.writeServerPEM(t)
	cases := []struct {
		name     string
		cert     string
		key      string
		wantPath string
	}{
		{"missing_cert", filepath.Join(dir, "nope.crt"), keyPath, "nope.crt"},
		{"missing_key", certPath, filepath.Join(dir, "nope.key"), "nope.key"},
		{"garbage_cert", garbage, keyPath, "garbage.pem"},
		{"garbage_key", certPath, garbage, "garbage.pem"},
		{"mismatched_pair", certPath, otherKeyPath, "controller.crt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := resolveTLSConfig(c.cert, c.key, discardLog())
			if err == nil {
				t.Fatalf("bad keypair must fail fast, got config %v", cfg)
			}
			if cfg != nil {
				t.Fatal("bad keypair must not return a usable TLS config")
			}
			if !strings.Contains(err.Error(), c.wantPath) {
				t.Fatalf("error %q must name the offending path %s", err, c.wantPath)
			}
		})
	}
}

// TestControllerServesAPIOverTLS is the integration proof: the config
// resolveTLSConfig returns (from generated PEM files on disk) serves the
// REAL API mux over a real TLS handshake. Health and metrics both answer on
// the SAME TLS listener; a client without the CA is rejected (no
// InsecureSkipVerify anywhere); a TLS 1.1-only client is refused (the 1.2
// floor is enforced, not just declared).
func TestControllerServesAPIOverTLS(t *testing.T) {
	pki := newTestPKI(t)
	certPath, keyPath := pki.writeServerPEM(t)
	tlsCfg, err := resolveTLSConfig(certPath, keyPath, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS 1.2 (%d)", tlsCfg.MinVersion, tls.VersionTLS12)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{
		Handler:           buildTestMux(t),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = httpSrv.ServeTLS(ln, "", "") }()
	defer httpSrv.Close()
	base := "https://" + ln.Addr().String()

	// Trusting client: real handshake against the generated cert.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pki.pool(t)},
	}}
	for _, path := range []string{"/api/health", "/metrics"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s over TLS: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s over TLS status = %d, want 200", path, resp.StatusCode)
		}
		if path == "/api/health" && !strings.Contains(string(body), "ok") {
			t.Fatalf("health body = %q, want status ok", body)
		}
		if path == "/metrics" && !strings.Contains(string(body), "encode_") {
			t.Fatalf("metrics body missing encode_ metrics: %q", body[:min(200, len(body))])
		}
	}

	// Untrusting client (system roots only): the handshake MUST fail —
	// the server presents a private-CA cert, nothing skips verification.
	badClient := &http.Client{Timeout: 5 * time.Second}
	if _, err := badClient.Get(base + "/api/health"); err == nil {
		t.Fatal("client without the CA must be rejected, got a successful response")
	}

	// TLS 1.1-only client: refused by the MinVersion floor. MinVersion is
	// REQUIRED here, not cosmetic: since Go 1.18 the client default minimum
	// is TLS 1.2, so a MaxVersion-only config fails locally ("no supported
	// versions satisfy MinVersion and MaxVersion") BEFORE any ClientHello —
	// the test would pass even with the server floor deleted. Pinning
	// MinVersion to 1.0 makes the client genuinely offer ≤1.1 on the wire.
	oldClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pki.pool(t), MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11},
	}, Timeout: 5 * time.Second}
	if _, err := oldClient.Get(base + "/api/health"); err == nil {
		t.Fatal("TLS 1.1 client must be refused (MinVersion 1.2)")
	}
}

// discardLog drops log output — for call sites that only care about the
// returned config/error, not the warnings.
func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestResolveTLSConfigValidCertNoWarning: a cert inside its validity window
// must not produce any boot warning.
func TestResolveTLSConfigValidCertNoWarning(t *testing.T) {
	pki := newTestPKI(t)
	certPath, keyPath := pki.writeServerPEM(t)
	var buf bytes.Buffer
	cfg, err := resolveTLSConfig(certPath, keyPath, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil || cfg == nil {
		t.Fatalf("valid keypair must load cleanly: cfg=%v err=%v", cfg, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("valid cert must not warn, got: %q", buf.String())
	}
}

// TestResolveTLSConfigInvalidValidityWarnsButStarts pins the boot-warning
// contract: an expired or not-yet-valid cert logs a WARNING (naming the path
// and NotAfter) but does NOT fail startup — the operator may be mid-rotation.
// The server still comes up and presents the cert: a trusting client fails at
// x509 VALIDITY (not connection refused), proving the TLS listener is serving.
func TestResolveTLSConfigInvalidValidityWarnsButStarts(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		wantWarn  string
	}{
		{"expired", now.Add(-48 * time.Hour), now.Add(-24 * time.Hour), "EXPIRED"},
		{"not_yet_valid", now.Add(24 * time.Hour), now.Add(48 * time.Hour), "NOT YET VALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pki := newTestPKIWithServerValidity(t, c.notBefore, c.notAfter)
			certPath, keyPath := pki.writeServerPEM(t)
			var buf bytes.Buffer
			cfg, err := resolveTLSConfig(certPath, keyPath, slog.New(slog.NewTextHandler(&buf, nil)))
			if err != nil {
				t.Fatalf("validity problems must warn, not fail startup: %v", err)
			}
			if cfg == nil {
				t.Fatal("want a usable TLS config despite the validity warning")
			}
			out := buf.String()
			if !strings.Contains(out, c.wantWarn) || !strings.Contains(out, certPath) || !strings.Contains(out, "not_after") {
				t.Fatalf("warning must name the problem, cert path and NotAfter; got: %q", out)
			}
			// Server still starts: serve the REAL mux over TLS with the
			// loaded config; the client's failure must be x509 validity,
			// not a dead listener.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			httpSrv := &http.Server{Handler: buildTestMux(t), TLSConfig: cfg, ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = httpSrv.ServeTLS(ln, "", "") }()
			defer httpSrv.Close()
			client := &http.Client{Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pki.pool(t)},
			}, Timeout: 5 * time.Second}
			_, err = client.Get("https://" + ln.Addr().String() + "/api/health")
			if err == nil {
				t.Fatal("client must reject a cert outside its validity window")
			}
			if !strings.Contains(err.Error(), "x509") || !strings.Contains(err.Error(), "valid") {
				t.Fatalf("want an x509 validity error (server up), got: %v", err)
			}
		})
	}
}
