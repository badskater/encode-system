package offsite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeSFTP is an in-memory sftpAPI — the client seam that keeps these tests
// free of a real SSH server. It records paths/flags and serves listings.
type fakeSFTP struct {
	files      map[string][]byte
	modes      map[string]os.FileMode
	mkdirs     []string
	opens      []string
	openFlags  []int
	removed    []string
	chmods     []fakeChmod
	renames    []fakeRename
	mkdirErr   error
	openErr    error
	readDirErr error
	removeErr  error
	chmodErr   error
	renameErr  error
	writeErr   error // injected mid-copy Write failure
	closeErr   error // injected flush-on-Close failure
	closed     int
}

type fakeChmod struct {
	path string
	mode os.FileMode
}

type fakeRename struct {
	old, new string
}

func newFakeSFTP() *fakeSFTP {
	return &fakeSFTP{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
}

func (f *fakeSFTP) MkdirAll(p string) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.mkdirs = append(f.mkdirs, p)
	return nil
}

func (f *fakeSFTP) OpenFile(p string, flags int) (io.WriteCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opens = append(f.opens, p)
	f.openFlags = append(f.openFlags, flags)
	return &fakeSFTPWriter{parent: f, path: p}, nil
}

func (f *fakeSFTP) ReadDir(p string) ([]os.FileInfo, error) {
	if f.readDirErr != nil {
		return nil, f.readDirErr
	}
	var out []os.FileInfo
	for name := range f.files {
		dir, base := filepath.Split(name)
		if strings.TrimSuffix(dir, "/") == strings.TrimSuffix(p, "/") || (p == "." && !strings.Contains(name, "/")) {
			out = append(out, fakeFileInfo{name: base})
		}
	}
	// A subdirectory entry must be skipped by List.
	out = append(out, fakeFileInfo{name: "subdir", dir: true})
	return out, nil
}

func (f *fakeSFTP) Remove(p string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.files, p)
	delete(f.modes, p)
	f.removed = append(f.removed, p)
	return nil
}

func (f *fakeSFTP) Chmod(p string, mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	f.chmods = append(f.chmods, fakeChmod{path: p, mode: mode})
	f.modes[p] = mode
	return nil
}

func (f *fakeSFTP) Rename(old, new string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	f.renames = append(f.renames, fakeRename{old: old, new: new})
	f.files[new] = f.files[old]
	delete(f.files, old)
	if m, ok := f.modes[old]; ok {
		f.modes[new] = m
		delete(f.modes, old)
	}
	return nil
}

func (f *fakeSFTP) Close() error { f.closed++; return nil }

type fakeSFTPWriter struct {
	parent *fakeSFTP
	path   string
	buf    bytes.Buffer
}

func (w *fakeSFTPWriter) Write(b []byte) (int, error) {
	if w.parent.writeErr != nil {
		return 0, w.parent.writeErr
	}
	return w.buf.Write(b)
}
func (w *fakeSFTPWriter) Close() error {
	if w.parent.closeErr != nil {
		return w.parent.closeErr
	}
	w.parent.files[w.path] = w.buf.Bytes()
	return nil
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

// TestSFTPDestinationUpload: Put creates the root, uploads to a TEMP name,
// chmods it 0600 and renames it onto the final name (Q1/Q7) — the exact
// local bytes land, no partial object remains.
func TestSFTPDestinationUpload(t *testing.T) {
	f := newFakeSFTP()
	d := &SFTPDestination{Client: f, Root: "/backups/encode"}
	dir := t.TempDir()
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, []byte("sftp-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(context.Background(), "snap.db", src); err != nil {
		t.Fatal(err)
	}
	if len(f.mkdirs) != 1 || f.mkdirs[0] != "/backups/encode" {
		t.Fatalf("MkdirAll must create the root: %v", f.mkdirs)
	}
	if len(f.opens) != 1 || !strings.HasPrefix(f.opens[0], "/backups/encode/snap.db.partial-") {
		t.Fatalf("upload must target a temp name: %v", f.opens)
	}
	if f.openFlags[0]&(os.O_CREATE|os.O_TRUNC) != os.O_CREATE|os.O_TRUNC {
		t.Fatalf("open flags must create+truncate: %d", f.openFlags[0])
	}
	if len(f.chmods) != 1 || f.chmods[0].path != f.opens[0] || f.chmods[0].mode != 0o600 {
		t.Fatalf("temp file must be chmodded 0600 before rename: %v", f.chmods)
	}
	if len(f.renames) != 1 || f.renames[0].old != f.opens[0] || f.renames[0].new != "/backups/encode/snap.db" {
		t.Fatalf("temp must rename onto the final name: %v", f.renames)
	}
	if string(f.files["/backups/encode/snap.db"]) != "sftp-bytes" {
		t.Fatalf("content wrong: %q", f.files["/backups/encode/snap.db"])
	}
	if len(f.files) != 1 {
		t.Fatalf("no partial object may remain: %v", f.files)
	}
}

// TestSFTPDestinationUploadErrors: mkdir and open failures propagate.
func TestSFTPDestinationUploadErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeSFTP()
	f.mkdirErr = errors.New("permission denied")
	d := &SFTPDestination{Client: f, Root: "/backups"}
	if err := d.Put(context.Background(), "snap.db", src); err == nil {
		t.Fatal("mkdir failure must propagate")
	}
	f2 := newFakeSFTP()
	f2.openErr = errors.New("disk full")
	d2 := &SFTPDestination{Client: f2, Root: "/backups"}
	if err := d2.Put(context.Background(), "snap.db", src); err == nil {
		t.Fatal("open failure must propagate")
	}
}

// TestSFTPDestinationListAndDelete: List returns file names (dirs skipped),
// Delete addresses root/name.
func TestSFTPDestinationListAndDelete(t *testing.T) {
	f := newFakeSFTP()
	f.files["/backups/a.db"] = []byte("a")
	f.files["/backups/b.db"+KeySuffix] = []byte("k")
	d := &SFTPDestination{Client: f, Root: "/backups"}
	names, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("List must return the 2 files, skip subdir: %v", names)
	}
	if err := d.Delete(context.Background(), "a.db"); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 1 || f.removed[0] != "/backups/a.db" {
		t.Fatalf("Delete path wrong: %v", f.removed)
	}
}

// TestSFTPDestinationTraversalGuard: hostile names cannot escape the root.
func TestSFTPDestinationTraversalGuard(t *testing.T) {
	d := &SFTPDestination{Client: newFakeSFTP(), Root: "/backups"}
	if err := d.Put(context.Background(), "../evil", "unused"); err == nil {
		t.Fatal("Put must reject traversal")
	}
	if err := d.Delete(context.Background(), "../evil"); err == nil {
		t.Fatal("Delete must reject traversal")
	}
}

// TestBuildSFTPAuthFailure: an auth failure from the dial seam becomes a
// build error Result — the other destination still builds.
func TestBuildSFTPAuthFailure(t *testing.T) {
	authErr := errors.New("ssh: handshake failed: unable to authenticate")
	dial := func(ctx context.Context, cfg model.BackupOffsiteSFTP) (sftpAPI, error) { return nil, authErr }
	st := &model.Settings{}
	st.BackupOffsite.S3 = model.BackupOffsiteS3{
		Enabled: true, Endpoint: "minio.example.com:9000", Bucket: "b",
		AccessKey: "AK" + "TEST", SecretKey: "SK" + "TEST",
	}
	st.BackupOffsite.SFTP = model.BackupOffsiteSFTP{
		Enabled: true, Host: "sftp.example.com", User: "backup", Password: "pw" + "TEST",
	}
	dests, errs := BuildDestinations(context.Background(), st, dial)
	if len(dests) != 1 || dests[0].Name() != "s3" {
		t.Fatalf("s3 must still build when sftp auth fails: %v", dests)
	}
	if len(errs) != 1 || errs[0].Destination != "sftp" || !strings.Contains(errs[0].Err, "authenticate") {
		t.Fatalf("auth failure must be a sftp error result: %v", errs)
	}
}

// TestBuildSFTPPassesConfigAndDefaultRoot: the dial seam receives the
// configured credentials; an empty Path defaults to "." (SSH home dir).
func TestBuildSFTPPassesConfigAndDefaultRoot(t *testing.T) {
	var got model.BackupOffsiteSFTP
	dial := func(ctx context.Context, cfg model.BackupOffsiteSFTP) (sftpAPI, error) {
		got = cfg
		return newFakeSFTP(), nil
	}
	cfg := model.BackupOffsiteSFTP{
		Enabled: true, Host: "sftp.example.com", Port: 2222, User: "backup",
		Password: "pw" + "TEST",
	}
	d, err := BuildSFTP(context.Background(), cfg, dial)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != cfg.Host || got.Port != 2222 || got.User != "backup" || got.Password != cfg.Password {
		t.Fatalf("dial seam got wrong config: %+v", got)
	}
	if d.Root != "." {
		t.Fatalf("empty path must default to '.': %q", d.Root)
	}
	if d.Close() != nil {
		t.Fatal("close")
	}
}

// TestDialSFTPNoCredentials: the production dialer refuses a config with
// neither password nor key — before any network I/O.
func TestDialSFTPNoCredentials(t *testing.T) {
	_, err := dialSFTP(context.Background(), model.BackupOffsiteSFTP{Host: "sftp.example.com", User: "u"})
	if err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("want no-credentials error, got %v", err)
	}
}

// TestDialSFTPParseKeyFailure: a malformed private key fails at parse time
// (no network). A real key parses and proceeds to the dial stage.
func TestDialSFTPParseKeyFailure(t *testing.T) {
	_, err := dialSFTP(context.Background(), model.BackupOffsiteSFTP{
		Host: "sftp.example.com", User: "u", PrivateKey: "not-a-pem-key",
	})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("want key parse error, got %v", err)
	}
}

// TestDialSFTPConnectionRefused: against a closed local port the dialer
// returns a bounded connection error (exercises the real dial path without
// an SSH server).
func TestDialSFTPConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // port is now guaranteed closed
	_, err = dialSFTP(context.Background(), model.BackupOffsiteSFTP{
		Host: "127.0.0.1", Port: port, User: "u", Password: "pw" + "TEST",
		HostKey: pinnedKey(genPub(t)),
	})
	if err == nil {
		t.Fatal("dial to a closed port must fail")
	}
	if !strings.Contains(err.Error(), "sftp dial") {
		t.Fatalf("want dial-stage error, got %v", err)
	}
}
