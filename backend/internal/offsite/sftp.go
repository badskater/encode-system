package offsite

import (
	"context"
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
	Close() error
}

// DialSFTP opens an SFTP session for the given config. Production passes
// nil (dialSFTP below); tests inject a fake returning an in-memory sftpAPI
// or an auth error.
type DialSFTP func(cfg model.BackupOffsiteSFTP) (sftpAPI, error)

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
	dst, err := d.Client.OpenFile(path.Join(d.Root, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	// SFTP buffers writes — Close flushes and is where write errors land.
	return dst.Close()
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
// dialer. An empty Path defaults to "." (the SSH user's home dir).
func BuildSFTP(cfg model.BackupOffsiteSFTP, dial DialSFTP) (*SFTPDestination, error) {
	if dial == nil {
		dial = dialSFTP
	}
	client, err := dial(cfg)
	if err != nil {
		return nil, err
	}
	root := cfg.Path
	if root == "" {
		root = "."
	}
	return &SFTPDestination{Client: client, Root: root}, nil
}

// BuildDestinations constructs every enabled destination from live
// settings. A destination that fails to build (bad key, dial/auth failure)
// becomes an error Result — the other destinations still run. dial is the
// SFTP connection seam (nil = production dialer).
func BuildDestinations(st *model.Settings, dial DialSFTP) ([]Destination, []Result) {
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
		d, err := BuildSFTP(o.SFTP, dial)
		if err != nil {
			errs = append(errs, Result{Destination: "sftp", Err: err.Error()})
		} else {
			dests = append(dests, d)
		}
	}
	return dests, errs
}

// sshTimeout bounds the TCP dial and the SSH handshake. Transfer hangs are
// bounded separately by Run's context (cancellation closes the connection).
const sshTimeout = 15 * time.Second

// dialSFTP is the production DialSFTP: TCP dial → SSH handshake → SFTP
// session. Auth offers the private key first (when set), then the password.
// Host keys are NOT pinned: the settings surface has no known_hosts field,
// so operators must use trusted networks/destinations (Operations.md states
// the tradeoff alongside the shares-key bundle warning).
func dialSFTP(cfg model.BackupOffsiteSFTP) (sftpAPI, error) {
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
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, sshTimeout)
	if err != nil {
		return nil, fmt.Errorf("sftp dial: %w", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
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
func (c *sftpConn) Close() error {
	_ = c.sftp.Close()
	return c.ssh.Close()
}
