package offsite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpAPI is the subset of *sftp.Client the destination uses — the test
// seam: unit tests inject an in-memory fake, so no real SSH server is
// needed (auth failures are exercised through the DialSFTP seam).
type sftpAPI interface {
	MkdirAll(path string) error
	OpenFile(path string, flags int) (io.WriteCloser, error)
	ReadDir(path string) ([]os.FileInfo, error)
	Remove(path string) error
	Chmod(path string, mode os.FileMode) error
	Rename(oldpath, newpath string) error
	Close() error
}

// DialSFTP opens an SFTP session for the given config. Production passes
// nil (dialSFTP below); tests inject a fake returning an in-memory sftpAPI
// or an auth error. The context bounds the whole construction (dial +
// handshake + subsystem exchange) — P4 review Q4.
type DialSFTP func(ctx context.Context, cfg model.BackupOffsiteSFTP) (sftpAPI, error)

// SFTPDestination stores snapshots under Root on an SFTP server. It owns
// the connection: Run closes it when the destination run finishes (or the
// context cancels, aborting hung transfers).
type SFTPDestination struct {
	Client sftpAPI
	Root   string // remote directory; "." = the SSH user's home dir
}

// Name identifies the destination in results and audit rows.
func (d *SFTPDestination) Name() string { return "sftp" }

// Put uploads one local file to Root/name, creating Root when missing.
// The upload is atomic-safe (P4 review Q7): bytes go to a unique temp name,
// are chmodded 0600 (Q1 — the bundle carries the shares key; server-default
// 0644 would expose it), and are renamed onto the final name only after a
// fully successful copy+flush. Any failure removes the partial, so a
// corrupt upload can never sit at the final name and be counted as a valid
// generation by retention.
func (d *SFTPDestination) Put(ctx context.Context, name, localPath string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := d.Client.MkdirAll(d.Root); err != nil {
		return fmt.Errorf("mkdir %s: %w", d.Root, err)
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := name + ".partial-" + partialSuffix()
	tmpPath := path.Join(d.Root, tmp)
	finalPath := path.Join(d.Root, name)
	dst, err := d.Client.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	// From here on, every failure path cleans the partial (best-effort).
	cleanup := func() { _ = d.Client.Remove(tmpPath) }
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		cleanup()
		return err
	}
	// SFTP buffers writes — Close flushes and is where write errors land.
	if err := dst.Close(); err != nil {
		cleanup()
		return err
	}
	if err := d.Client.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := d.Client.Rename(tmpPath, finalPath); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", tmpPath, err)
	}
	return nil
}

// partialSuffix returns a short random hex tag making temp names unique per
// upload, so overlapping uploads of the same snapshot (two HA replicas)
// never write into each other's temp file.
func partialSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails in practice; fall back to the pid so a
		// temp name still exists (uniqueness degrades, safety does not).
		return strconv.Itoa(os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// List returns the file names directly under Root (directories skipped).
func (d *SFTPDestination) List(ctx context.Context) ([]string, error) {
	infos, err := d.Client.ReadDir(d.Root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, fi := range infos {
		if !fi.IsDir() {
			names = append(names, fi.Name())
		}
	}
	return names, nil
}

// Delete removes one object under Root.
func (d *SFTPDestination) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return d.Client.Remove(path.Join(d.Root, name))
}

// Close releases the SFTP session and the underlying SSH connection.
func (d *SFTPDestination) Close() error { return d.Client.Close() }

// BuildSFTP dials and wraps one SFTP destination. dial nil = production
// dialer. An empty Path defaults to "." (the SSH user's home dir). The dial
// runs under ctx: construction is bounded even when the seam ignores the
// context (P4 review Q4 — BuildDestinations used to run OUTSIDE the run
// bound, so a stalling server hung the hook indefinitely).
func BuildSFTP(ctx context.Context, cfg model.BackupOffsiteSFTP, dial DialSFTP) (*SFTPDestination, error) {
	if dial == nil {
		dial = dialSFTP
	}
	type dialResult struct {
		client sftpAPI
		err    error
	}
	ch := make(chan dialResult, 1)
	go func() {
		c, err := dial(ctx, cfg)
		ch <- dialResult{c, err}
	}()
	var client sftpAPI
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		client = r.client
	case <-ctx.Done():
		// The dial may still complete later — close whatever it returns
		// so an abandoned connection never leaks.
		go func() {
			if r := <-ch; r.client != nil {
				r.client.Close()
			}
		}()
		return nil, fmt.Errorf("sftp dial: %w", ctx.Err())
	}
	root := cfg.Path
	if root == "" {
		root = "."
	}
	return &SFTPDestination{Client: client, Root: root}, nil
}

// BuildDestinations constructs every enabled destination from live
// settings, bounded by ctx (P4 review Q4). A destination that fails to
// build (bad key, dial/auth failure, timeout) becomes an error Result —
// the other destinations still run. dial is the SFTP connection seam
// (nil = production dialer).
func BuildDestinations(ctx context.Context, st *model.Settings, dial DialSFTP) ([]Destination, []Result) {
	var dests []Destination
	var errs []Result
	o := st.BackupOffsite
	if o.S3.Enabled {
		d, err := BuildS3(o.S3)
		if err != nil {
			errs = append(errs, Result{Destination: "s3", Err: err.Error()})
		} else {
			dests = append(dests, d)
		}
	}
	if o.SFTP.Enabled {
		d, err := BuildSFTP(ctx, o.SFTP, dial)
		if err != nil {
			errs = append(errs, Result{Destination: "sftp", Err: err.Error()})
		} else {
			dests = append(dests, d)
		}
	}
	return dests, errs
}

// sshTimeout bounds the TCP dial, the SSH handshake AND the SFTP subsystem
// exchange (the conn deadline stays set across all three). Transfer hangs
// are bounded separately by Run's context (cancellation closes the
// connection). A var so tests can shorten it.
var sshTimeout = 15 * time.Second

// dialSFTP is the production DialSFTP: TCP dial → SSH handshake → SFTP
// session, all under ctx and a conn deadline. Auth offers the private key
// first (when set), then the password. The server's host key is PINNED
// (P4 review D1): the bundle carries the shares key, so an unpinned
// connection would let a MITM steal it. host_key is required configuration
// (validated at save time; re-checked here as defense in depth).
func dialSFTP(ctx context.Context, cfg model.BackupOffsiteSFTP) (sftpAPI, error) {
	var auths []ssh.AuthMethod
	if cfg.PrivateKey != "" {
		key, err := ssh.ParseRawPrivateKey([]byte(cfg.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("sftp private key: %w", err)
		}
		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			return nil, fmt.Errorf("sftp signer: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		auths = append(auths, ssh.Password(cfg.Password))
	}
	if len(auths) == 0 {
		return nil, errors.New("sftp: no credentials configured")
	}
	if cfg.HostKey == "" {
		return nil, errors.New("sftp: host_key is required — pin the server's host key (get it with: ssh-keyscan <host>)")
	}
	hostKey, err := ParseHostKey(cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("sftp host_key: %w", err)
	}
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: sshTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sftp dial: %w", err)
	}
	// One deadline across the handshake AND the subsystem exchange:
	// sftp.NewClient does its own I/O on the same conn after the handshake,
	// and a stalling server must not hang either phase (Q4).
	deadline := time.Now().Add(sshTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	// ctx cancellation aborts a blocked handshake immediately.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         sshTimeout,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sftp ssh handshake: %w", err)
	}
	sshClient := ssh.NewClient(c, chans, reqs)
	sc, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("sftp session: %w", err)
	}
	// Construction done: clear the deadline — transfers are bounded by
	// Run's ctx (which closes the connection on cancellation).
	_ = conn.SetDeadline(time.Time{})
	return &sftpConn{ssh: sshClient, sftp: sc}, nil
}

// sftpConn adapts *sftp.Client to the sftpAPI seam. *sftp.File satisfies
// io.WriteCloser, so OpenFile's concrete return converts directly.
type sftpConn struct {
	ssh  *ssh.Client
	sftp *sftp.Client
}

func (c *sftpConn) MkdirAll(p string) error { return c.sftp.MkdirAll(p) }
func (c *sftpConn) OpenFile(p string, flags int) (io.WriteCloser, error) {
	return c.sftp.OpenFile(p, flags)
}
func (c *sftpConn) ReadDir(p string) ([]os.FileInfo, error) { return c.sftp.ReadDir(p) }
func (c *sftpConn) Remove(p string) error                   { return c.sftp.Remove(p) }
func (c *sftpConn) Chmod(p string, mode os.FileMode) error  { return c.sftp.Chmod(p, mode) }

// Rename uses the posix-rename@openssh.com extension: it atomically
// REPLACES an existing target, keeping same-name re-uploads (two HA
// replicas racing on one snapshot) idempotent. Plain SSH_FXP_RENAME would
// fail on an existing target.
func (c *sftpConn) Rename(old, new string) error { return c.sftp.PosixRename(old, new) }
func (c *sftpConn) Close() error {
	_ = c.sftp.Close()
	return c.ssh.Close()
}
