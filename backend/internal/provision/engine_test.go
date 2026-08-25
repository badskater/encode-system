package provision

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

func TestRequestValidate(t *testing.T) {
	base := func() Request {
		return Request{
			Host: "172.24.92.250", Port: 5985, Scheme: "http",
			WinRMUser: "Administrator", WinRMPassword: "secret", NodeName: "enc-03",
		}
	}
	r := base()
	if err := r.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"host with spaces", func(r *Request) { r.Host = "bad host" }},
		{"host shell metachars", func(r *Request) { r.Host = "host;rm" }},
		{"empty host", func(r *Request) { r.Host = "" }},
		{"port zero defaults", func(r *Request) { r.Port = 0 }}, // valid: defaults to 5985
		{"port out of range", func(r *Request) { r.Port = 99999 }},
		{"bad scheme", func(r *Request) { r.Scheme = "ftp" }},
		{"no password", func(r *Request) { r.WinRMPassword = "" }},
		{"no node name", func(r *Request) { r.NodeName = "" }},
		{"node name metachars", func(r *Request) { r.NodeName = "a;b" }},
	}
	for _, tc := range cases {
		r := base()
		tc.mutate(&r)
		err := r.Validate()
		if tc.name == "port zero defaults" {
			if err != nil || r.Port != 5985 {
				t.Errorf("%s: expected default port, got err=%v port=%d", tc.name, err, r.Port)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

func TestBuildVars(t *testing.T) {
	settings := &model.Settings{
		ControllerURL:  "http://172.24.92.232:8080",
		NFSServer:      "unraid01",
		ScriptsShare:   "/mnt/user/scripts",
		ReleaseShare:   "/mnt/user/ReleaseFolders",
		NodeBinDir:     `C:\bin`,
		NodeScriptsDir: `C:\Encodes\scripts`,
		NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
	}
	req := Request{
		Host: "172.24.92.250", Port: 5985, Scheme: "http",
		WinRMUser: "Administrator", WinRMPassword: "s3cret!pass",
		NodeName: "enc-03", MountNFS: true, InstallToolchain: true,
	}
	vars := buildVars(settings, req, "deadbeefpairingcode", true, true)

	for _, want := range []string{
		"encode_controller_url: 'http://172.24.92.232:8080'",
		"encode_node_name: 'enc-03'",
		"encode_nfs_scripts_export: 'unraid01:/mnt/user/scripts'",
		"encode_nfs_release_export: 'unraid01:/mnt/user/ReleaseFolders'",
		"encode_mount_nfs: true",
		"encode_install_toolchain: true",
		"encode_push_bin: true",
		"encode_has_lib: true",
		"encode_pairing_code: 'deadbeefpairingcode'",
		"ansible_user: 'Administrator'",
		"ansible_password: 's3cret!pass'",
	} {
		if !strings.Contains(vars, want) {
			t.Errorf("vars missing %q\ngot:\n%s", want, vars)
		}
	}
}

func TestBuildVarsNFSDisabledWithoutServer(t *testing.T) {
	settings := &model.Settings{ControllerURL: "http://c:8080", NodeBinDir: `C:\bin`}
	req := Request{Host: "h", WinRMUser: "u", WinRMPassword: "p", NodeName: "n", MountNFS: true}
	vars := buildVars(settings, req, "code", false, false)
	if !strings.Contains(vars, "encode_mount_nfs: false") {
		t.Errorf("NFS must stay off without a server configured:\n%s", vars)
	}
	if !strings.Contains(vars, "encode_has_lib: false") {
		t.Errorf("has_lib must be false:\n%s", vars)
	}
}

func TestYamlQuoteEscapesSingleQuotes(t *testing.T) {
	got := yamlQuote("it's a 'test'")
	if got != "'it''s a ''test'''" {
		t.Errorf("bad escaping: %s", got)
	}
	// A password with quotes must survive intact for ansible to parse.
	got = yamlQuote(`p'a"s\s`)
	if got != `'p''a"s\s'` {
		t.Errorf("bad escaping: %s", got)
	}
}

// fakeProvStore is a minimal Store for engine-level tests.
type fakeProvStore struct {
	logs map[int64]string
}

func (f *fakeProvStore) GetSettings(ctx context.Context) (*model.Settings, error) {
	return &model.Settings{ControllerURL: "http://c:8080"}, nil
}
func (f *fakeProvStore) CreateProvisionRun(ctx context.Context, pr *model.ProvisionRun) (*model.ProvisionRun, error) {
	pr.ID = 1
	return pr, nil
}
func (f *fakeProvStore) GetProvisionRun(ctx context.Context, id int64) (*model.ProvisionRun, error) {
	return &model.ProvisionRun{ID: id, Status: "running"}, nil
}
func (f *fakeProvStore) ListProvisionRuns(ctx context.Context) ([]*model.ProvisionRun, error) {
	return nil, nil
}
func (f *fakeProvStore) SetProvisionRunStatus(ctx context.Context, id int64, status, errMsg string, finished bool) error {
	return nil
}
func (f *fakeProvStore) AppendProvisionRunLog(ctx context.Context, id int64, chunk string) error {
	if f.logs == nil {
		f.logs = map[int64]string{}
	}
	f.logs[id] += chunk
	return nil
}

// Regression: streamCommand must terminate when the child process exits.
// The original io.Pipe implementation deadlocked forever on a finished
// process (the write end was never closed, so the scanner waited for an
// EOF that would never come).
func TestStreamCommandTerminatesAfterChildExit(t *testing.T) {
	st := &fakeProvStore{}
	cmd := exec.Command("sh", "-c", "echo hello; echo world >&2; echo done")
	done := make(chan error, 1)
	go func() {
		done <- streamCommand(context.Background(), context.Background(), st, 1, cmd, 1024*1024)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamCommand error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: streamCommand did not return after the child exited")
	}
	logged := st.logs[1]
	for _, want := range []string{"hello", "world", "done"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q (got %q)", want, logged)
		}
	}
}

func TestHostReAllowsRealisticTargets(t *testing.T) {
	good := []string{"172.24.92.250", "enc-node-3.lan", "::1", "[fd00::5]", "host_name"}
	for _, g := range good {
		if !hostRe.MatchString(g) {
			t.Errorf("%q should be accepted", g)
		}
	}
	bad := []string{"", "a b", "a;id", "a$(x)", "a`x`", "a|b", "a>b"}
	for _, b := range bad {
		if hostRe.MatchString(b) {
			t.Errorf("%q should be rejected", b)
		}
	}
}
