package offsite

// D1/Q4 regressions against a REAL in-process SSH+SFTP server (x/crypto/ssh
// server side + pkg/sftp server): host-key pinning is exercised through an
// actual handshake, not a mock; the bounded-dial regressions use stalling
// TCP listeners. No external SSH daemon is needed.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// testSSHServer starts an in-process SSH server serving the SFTP subsystem
// against the real filesystem. It returns the host:port and the server's
// host PUBLIC key. Password auth accepts anything (test-only).
func testSSHServer(t *testing.T) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serveSSHConn(conn, cfg)
		}
	}()
	return l.Addr().String(), signer.PublicKey()
}

func serveSSHConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer channel.Close()
			for req := range requests {
				if req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp" {
					if req.Reply(true, nil) != nil {
						return
					}
					srv, err := sftp.NewServer(channel)
					if err != nil {
						return
					}
					srv.Serve() // runs until the client closes the channel
					return
				}
				req.Reply(false, nil)
			}
		}()
	}
}

// hostPort splits a test server address into the SFTP config fields.
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, port
}

func pinnedKey(k ssh.PublicKey) string {
	return k.Type() + " " + base64.StdEncoding.EncodeToString(k.Marshal())
}

// TestDialSFTPHostKeyPinnedMatch: with the CORRECT host key pinned the real
// handshake succeeds and a full upload round-trips (temp+rename+chmod).
func TestDialSFTPHostKeyPinnedMatch(t *testing.T) {
	addr, hk := testSSHServer(t)
	host, port := hostPort(t, addr)
	root := t.TempDir()
	cfg := model.BackupOffsiteSFTP{
		Enabled: true, Host: host, Port: port, User: "backup",
		Password: "pw" + "TEST", Path: root, HostKey: pinnedKey(hk),
	}
	d, err := BuildSFTP(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("matching pinned host key must be accepted: %v", err)
	}
	defer d.Close()
	src := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(src, []byte("real-server-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "encode-20261111-120000-sched.db"
	if err := d.Put(context.Background(), name, src); err != nil {
		t.Fatalf("upload over real SFTP: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(got) != "real-server-bytes" {
		t.Fatalf("uploaded bytes wrong: %q %v", got, err)
	}
	// No partial left behind; the final file is 0600 (Q1/Q7 on a real server).
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".partial-") {
			t.Fatalf("partial left behind: %s", e.Name())
		}
	}
	if runtime.GOOS != "windows" { // POSIX perms are meaningless on Windows
		fi, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("uploaded file mode = %o, want 0600", fi.Mode().Perm())
		}
	}
}

// TestDialSFTPHostKeyMismatch: a DIFFERENT pinned key must abort the
// handshake — this is the MITM the pinning exists to stop.
func TestDialSFTPHostKeyMismatch(t *testing.T) {
	addr, _ := testSSHServer(t)
	host, port := hostPort(t, addr)
	otherPubRaw, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, err := ssh.NewPublicKey(otherPubRaw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.BackupOffsiteSFTP{
		Host: host, Port: port, User: "backup", Password: "pw" + "TEST",
		HostKey: pinnedKey(otherPub),
	}
	_, err = dialSFTP(context.Background(), cfg)
	if err == nil {
		t.Fatal("mismatched host key MUST be rejected")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("want handshake failure, got: %v", err)
	}
}

// TestDialSFTPHostKeyMissing: the production dialer refuses an unpinned
// config before any network I/O (defense in depth behind save validation).
func TestDialSFTPHostKeyMissing(t *testing.T) {
	_, err := dialSFTP(context.Background(), model.BackupOffsiteSFTP{
		Host: "sftp.example.com", User: "u", Password: "pw" + "TEST",
	})
	if err == nil || !strings.Contains(err.Error(), "host_key") {
		t.Fatalf("missing host key must be refused, got: %v", err)
	}
}

// TestDialSFTPStalledServerBounded: a TCP peer that accepts but never sends
// an SSH banner must not hang the dialer — the handshake deadline bounds it
// (Q4 probe: the old code hung past 5s; sshTimeout is shortened here).
func TestDialSFTPStalledServerBounded(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// Stall: hold the connection open, never write a banner.
			go func(c net.Conn) {
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}(conn)
		}
	}()
	host, port := hostPort(t, l.Addr().String())

	old := sshTimeout
	sshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sshTimeout = old })

	_, hk := testSSHServer(t) // any valid key — the handshake never starts
	start := time.Now()
	_, err = dialSFTP(context.Background(), model.BackupOffsiteSFTP{
		Host: host, Port: port, User: "u", Password: "pw" + "TEST", HostKey: pinnedKey(hk),
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stalled server must produce a dial/handshake error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("dial to a stalling server took %v — not bounded", elapsed)
	}
}

// TestBuildDestinationsDialHangBounded: a dial seam that blocks until ctx
// cancellation must not hold destination construction past the bound, and
// must not leak its goroutine (Q4: construction runs INSIDE the run bound).
func TestBuildDestinationsDialHangBounded(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()
	st := &model.Settings{}
	st.BackupOffsite.SFTP = model.BackupOffsiteSFTP{
		Enabled: true, Host: "sftp.example.com", User: "u", Password: "pw" + "TEST",
		HostKey: "ssh-ed25519 AAAA",
	}
	dial := func(ctx context.Context, cfg model.BackupOffsiteSFTP) (sftpAPI, error) {
		<-ctx.Done() // simulate a hung dial that only ctx can abort
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	dests, errs := BuildDestinations(ctx, st, dial)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("construction took %v — dial hang not bounded", elapsed)
	}
	if len(dests) != 0 || len(errs) != 1 || errs[0].Destination != "sftp" || errs[0].Err == "" {
		t.Fatalf("hung dial must become an sftp error result: dests=%v errs=%v", dests, errs)
	}
	// The seam goroutine unblocks on ctx.Done — it must exit promptly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseGoroutines {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline %d, now %d", baseGoroutines, runtime.NumGoroutine())
}

// TestBuildSFTPCancelledContext: a pre-cancelled context fails construction
// cleanly (no panic, no destination).
func TestBuildSFTPCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &model.Settings{}
	st.BackupOffsite.SFTP = model.BackupOffsiteSFTP{
		Enabled: true, Host: "h", User: "u", Password: "pw" + "TEST", HostKey: "ssh-ed25519 AAAA",
	}
	dial := func(ctx context.Context, cfg model.BackupOffsiteSFTP) (sftpAPI, error) {
		return nil, fmt.Errorf("should not get here: %w", ctx.Err())
	}
	dests, errs := BuildDestinations(ctx, st, dial)
	if len(dests) != 0 || len(errs) != 1 || errs[0].Err == "" {
		t.Fatalf("cancelled ctx must fail construction: %v %v", dests, errs)
	}
}
