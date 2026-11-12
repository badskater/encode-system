package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPKI is a throwaway CA + server keypair generated at RUNTIME (never
// committed, never a PEM literal in source). Mirrors the controller-side
// test helper; the two packages cannot share test code without an exported
// testutil package, which this small surface does not justify.
type testPKI struct {
	caCert    *x509.Certificate
	serverDER []byte
	serverKey *ecdsa.PrivateKey
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "encode-agent-test CA"},
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
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{caCert: caCert, serverDER: srvDER, serverKey: srvKey}
}

// writeCAFile writes the CA PEM where ENCODE_AGENT_CA_CERT can point at it.
func (p *testPKI) writeCAFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "encode-ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// startTLSServer serves h over a REAL TLS handshake with the test PKI's
// server cert. httptest.StartTLS cannot be used: it force-installs its own
// internal localhost certificate, which would defeat the point of testing
// custom-CA trust.
func startTLSServer(t *testing.T, pki *testPKI, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: h,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{pki.serverDER}, PrivateKey: pki.serverKey}},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String()
}

func agentCfg(url, dir string) Config {
	return Config{ControllerURL: url, NodeName: "ca-test-node", Token: nodeTok(), DataDir: dir}
}

// TestAgentCATrustsControllerTLS proves the whole point of the feature: with
// ENCODE_AGENT_CA_CERT set, BOTH controller-facing paths — the heartbeat
// (postJSON) and an update download (getAuth) — complete a real TLS handshake
// against a CA-signed controller cert, WITHOUT any HTTP-update opt-in (the
// URL is https, so the plain-HTTP gate is irrelevant).
func TestAgentCATrustsControllerTLS(t *testing.T) {
	t.Setenv("ENCODE_AGENT_ALLOW_HTTP_UPDATES", "") // must NOT be needed for https
	pki := newTestPKI(t)
	t.Setenv("ENCODE_AGENT_CA_CERT", pki.writeCAFile(t))

	var heartbeats, downloads int
	base := startTLSServer(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/heartbeat":
			heartbeats++
			w.Write([]byte(`{}`)) // empty reply = instruction "none"
		case "/api/agent/download/lib":
			downloads++
			w.Write([]byte("lib-over-tls"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	a, err := New(agentCfg(base, t.TempDir()), "1.0.0", testLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat over TLS with custom CA: %v", err)
	}
	var buf limitedBuffer
	if err := a.getAuth(context.Background(), "/api/agent/download/lib", &buf); err != nil {
		t.Fatalf("update download over TLS with custom CA: %v", err)
	}
	if got := buf.Bytes(); string(got) != "lib-over-tls" {
		t.Fatalf("download body = %q, want lib-over-tls", got)
	}
	if heartbeats != 1 || downloads != 1 {
		t.Fatalf("hits: heartbeats=%d downloads=%d, want 1/1", heartbeats, downloads)
	}
}

// TestAgentRejectsUntrustedControllerTLS is the negative control: the CA file
// is valid, but the controller presents a cert from a DIFFERENT authority.
// The handshake must fail — no InsecureSkipVerify, no silent trust widening.
func TestAgentRejectsUntrustedControllerTLS(t *testing.T) {
	trusted := newTestPKI(t)
	untrusted := newTestPKI(t)
	t.Setenv("ENCODE_AGENT_CA_CERT", trusted.writeCAFile(t))
	base := startTLSServer(t, untrusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	a, err := New(agentCfg(base, t.TempDir()), "1.0.0", testLog())
	if err != nil {
		t.Fatal(err) // the CA file itself is valid; trust fails at handshake time
	}
	err = a.heartbeat(context.Background())
	if err == nil {
		t.Fatal("heartbeat against an untrusted cert must fail")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("error should be a certificate-verification failure, got: %v", err)
	}
}

// TestAgentCAFailFast covers every broken ENCODE_AGENT_CA_CERT input: the
// agent must refuse to START (New returns an error naming the variable),
// never run with a half-configured trust store.
func TestAgentCAFailFast(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(garbage, []byte("<html>not a cert</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.crt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
	}{
		{"missing_file", filepath.Join(dir, "nope.crt")},
		{"garbage_pem", garbage},
		{"empty_file", empty},
		{"directory_as_path", dir},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ENCODE_AGENT_CA_CERT", c.path)
			_, err := New(agentCfg("https://controller.invalid", t.TempDir()), "1.0.0", testLog())
			if err == nil {
				t.Fatal("broken CA config must fail fast at New")
			}
			if !strings.Contains(err.Error(), "ENCODE_AGENT_CA_CERT") {
				t.Fatalf("error %q must name ENCODE_AGENT_CA_CERT", err)
			}
		})
	}
}

// TestAgentCAUnsetKeepsDefaultClient is the zero-change regression: without
// the env var the client is the plain default (nil Transport = system roots,
// default tuning) and plain-HTTP heartbeats work exactly as before.
func TestAgentCAUnsetKeepsDefaultClient(t *testing.T) {
	t.Setenv("ENCODE_AGENT_CA_CERT", "")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	a, err := New(agentCfg(ts.URL, t.TempDir()), "1.0.0", testLog())
	if err != nil {
		t.Fatal(err)
	}
	if a.Client.Transport != nil {
		t.Fatal("without ENCODE_AGENT_CA_CERT the client must keep the default transport")
	}
	if err := a.heartbeat(context.Background()); err != nil {
		t.Fatalf("plain-HTTP heartbeat regression: %v", err)
	}
}

// verifyLeaf checks a server certificate against a root pool the same way
// the TLS stack does (server-auth usage + hostname).
func verifyLeaf(t *testing.T, pool *x509.CertPool, leafDER []byte, wantOK bool) {
	t.Helper()
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"})
	if wantOK && err != nil {
		t.Fatalf("leaf must verify against the pool: %v", err)
	}
	if !wantOK && err == nil {
		t.Fatal("leaf from an unrelated CA must NOT verify against the pool")
	}
}

// TestLoadAgentCAPoolAppendsToSystemRoots proves the APPEND contract with a
// controlled stand-in for the system pool (injected via the systemCertPool
// seam): a pre-existing "public" root keeps validating its chains after the
// custom CA is appended, the custom CA's chain validates too, and an
// unrelated CA is still rejected. This is the "system-pool append doesn't
// break public-CA validation" property, tested without network access.
func TestLoadAgentCAPoolAppendsToSystemRoots(t *testing.T) {
	public := newTestPKI(t) // stand-in for a public root already in the system pool
	custom := newTestPKI(t) // the farm CA from ENCODE_AGENT_CA_CERT
	standIn := x509.NewCertPool()
	standIn.AddCert(public.caCert)
	orig := systemCertPool
	systemCertPool = func() (*x509.CertPool, error) { return standIn, nil }
	t.Cleanup(func() { systemCertPool = orig })

	pool, err := loadAgentCAPool(custom.writeCAFile(t))
	if err != nil {
		t.Fatal(err)
	}
	verifyLeaf(t, pool, custom.serverDER, true) // appended farm CA works
	verifyLeaf(t, pool, public.serverDER, true) // pre-existing public root untouched
	unrelated := newTestPKI(t)
	verifyLeaf(t, pool, unrelated.serverDER, false) // no trust widening
}

// TestLoadAgentCAPoolSystemPoolError: a broken system trust store is a
// fail-fast error, not a silent fallback to an empty (or custom-only) pool.
func TestLoadAgentCAPoolSystemPoolError(t *testing.T) {
	pki := newTestPKI(t)
	orig := systemCertPool
	systemCertPool = func() (*x509.CertPool, error) { return nil, io.ErrUnexpectedEOF }
	t.Cleanup(func() { systemCertPool = orig })
	if _, err := loadAgentCAPool(pki.writeCAFile(t)); err == nil {
		t.Fatal("system pool failure must propagate")
	}
}

// TestLoadAgentCAPoolRealSystemRoots exercises the UNMOCKED path on the test
// host: the real x509.SystemCertPool loads, the custom CA appends, and the
// farm cert verifies against the combined pool.
func TestLoadAgentCAPoolRealSystemRoots(t *testing.T) {
	pki := newTestPKI(t)
	pool, err := loadAgentCAPool(pki.writeCAFile(t))
	if err != nil {
		t.Fatalf("real system pool + append: %v", err)
	}
	verifyLeaf(t, pool, pki.serverDER, true)
}

// TestAgentHTTPUpdateGateUnchangedWithCA pins the compat contract: setting a
// CA file does NOT relax the plain-HTTP update gate. An http:// controller
// still refuses downloads without ENCODE_AGENT_ALLOW_HTTP_UPDATES=1, and the
// opt-in still works — the CA env and the HTTP gate are independent.
func TestAgentHTTPUpdateGateUnchangedWithCA(t *testing.T) {
	pki := newTestPKI(t)
	t.Setenv("ENCODE_AGENT_CA_CERT", pki.writeCAFile(t))
	t.Setenv("ENCODE_AGENT_ALLOW_HTTP_UPDATES", "")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("payload"))
	}))
	defer ts.Close()
	a, err := New(agentCfg(ts.URL, t.TempDir()), "1.0.0", testLog())
	if err != nil {
		t.Fatal(err)
	}
	var buf limitedBuffer
	err = a.getAuth(context.Background(), "/api/agent/download/agent", &buf)
	if err == nil || !strings.Contains(err.Error(), "refusing download") {
		t.Fatalf("plain-HTTP gate must still refuse with a CA configured, got: %v", err)
	}
	t.Setenv("ENCODE_AGENT_ALLOW_HTTP_UPDATES", "1")
	if err := a.getAuth(context.Background(), "/api/agent/download/agent", &buf); err != nil {
		t.Fatalf("opt-in must still allow plain-HTTP downloads: %v", err)
	}
}
