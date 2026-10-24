package provision

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// linuxReq is a reusable Linux provisioning request for tests.
func linuxReq() Request {
	return Request{
		Host: "10.0.0.5", Port: 22, Platform: "linux",
		SSHUser: "deploy", SSHPassword: "ssh-pa$$word", NodeName: "lnx-01",
		MountShares: true,
	}
}

// --- Request.Validate: Linux platform requires SSH connection details ---

func TestRequestValidateLinux(t *testing.T) {
	// Full valid Linux request.
	r := linuxReq()
	if err := (&r).Validate(); err != nil {
		t.Fatalf("valid linux request rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"linux no user", func(r *Request) { r.SSHUser = "" }},
		{"linux no auth", func(r *Request) { r.SSHPassword = ""; r.SSHKeyFile = "" }},
		{"linux no host", func(r *Request) { r.Host = "" }},
		{"linux no node name", func(r *Request) { r.NodeName = "" }},
	}
	for _, tc := range cases {
		r := linuxReq()
		tc.mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}

	// Key-file auth is a valid alternative to password auth.
	r = linuxReq()
	r.SSHPassword = ""
	r.SSHKeyFile = "/home/deploy/.ssh/id_ed25519"
	if err := r.Validate(); err != nil {
		t.Errorf("key-file auth should be valid: %v", err)
	}
}

// Linux default port is 22 when zero is passed.
func TestRequestValidateLinuxDefaultPort(t *testing.T) {
	r := linuxReq()
	r.Port = 0
	if err := (&r).Validate(); err != nil {
		t.Fatalf("default port rejected: %v", err)
	}
	if r.Port != 22 {
		t.Errorf("linux default port = %d, want 22", r.Port)
	}
}

// Windows requests keep working EXACTLY as before: Platform empty == windows.
func TestRequestValidateWindowsUnchanged(t *testing.T) {
	// Empty Platform must behave as the historical Windows path: WinRM
	// password required, SSH fields ignored.
	r := Request{
		Host: "172.24.92.250", Port: 5985, Scheme: "http",
		WinRMUser: "Administrator", WinRMPassword: "secret", NodeName: "enc-03",
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("windows request (empty platform) rejected: %v", err)
	}
	if r.Platform != "windows" {
		t.Errorf("empty platform normalized to %q, want windows", r.Platform)
	}
	// Explicit windows platform.
	r.Platform = "windows"
	if err := (&r).Validate(); err != nil {
		t.Fatalf("explicit windows request rejected: %v", err)
	}
}

// --- buildVars: Linux shape (cifs vars present, no WinRM vars) ---
//
// buildVars dispatches on Platform: Linux requests get a JSON vars file
// (avoids the $-in-password pitfall of env/YAML command-line passing),
// Windows requests keep their YAML shape unchanged.
func TestBuildVarsLinuxCIFS(t *testing.T) {
	settings := &model.Settings{
		ControllerURL:  "http://controller:8080",
		NodeBinDir:     "/opt/encode/bin",
		NodeScriptsDir: "/mnt/encodes/scripts",
		NodeReleaseDir: "/mnt/encodes/release",
	}
	scripts := &model.Share{
		Name: "smb-scripts", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "nas01", Path: "scripts", Username: "encode", Password: "c!f$pa$$",
		Enabled: true, MountPath: "/mnt/encodes/scripts",
	}
	release := &model.Share{
		Name: "smb-release", Kind: model.ShareSMB, Role: model.ShareRoleRelease,
		Server: "nas01", Path: "release", Username: "encode", Password: "c!f$pa$$",
		Enabled: true, MountPath: "/mnt/encodes/release",
	}
	req := linuxReq()
	req.MountShares = true
	vars := buildVars(settings, req, "paircode123", true, true, scripts, release)

	for _, want := range []string{
		`"encode_controller_url":"http://controller:8080"`,
		`"encode_node_name":"lnx-01"`,
		`"encode_cifs_scripts_server":"nas01"`,
		`"encode_cifs_scripts_share":"scripts"`,
		`"encode_cifs_scripts_user":"encode"`,
		`"encode_cifs_scripts_pass":"c!f$pa$$"`,
		`"encode_cifs_release_server":"nas01"`,
		`"encode_cifs_release_share":"release"`,
		`"encode_cifs_release_pass":"c!f$pa$$"`,
		`"encode_mount_cifs":true`,
		`"encode_cifs_vers":"3.0"`,
		`"encode_agent_user":"encode"`,
		`"encode_pairing_code":"paircode123"`,
	} {
		if !strings.Contains(vars, want) {
			t.Errorf("vars missing %q\ngot:\n%s", want, vars)
		}
	}

	// Linux vars must NOT carry any WinRM transport variables.
	for _, banned := range []string{
		"ansible_winrm_scheme",
		"ansible_winrm_transport",
		"ansible_winrm_server_cert_validation",
		"ansible_winrm_read_timeout_sec",
		"encode_winrm_https",
	} {
		if strings.Contains(vars, banned) {
			t.Errorf("linux vars must not contain %q\ngot:\n%s", banned, vars)
		}
	}
	// SSH connection vars must be present (ansible_password or key file).
	if !strings.Contains(vars, `"ansible_user":"deploy"`) {
		t.Errorf("ansible_user missing:\n%s", vars)
	}
	if !strings.Contains(vars, `"ansible_host":"10.0.0.5"`) {
		t.Errorf("ansible_host missing:\n%s", vars)
	}
	if !strings.Contains(vars, `"ansible_become":true`) {
		t.Errorf("ansible_become missing:\n%s", vars)
	}
	// SSH password (not key file) → ansible_password present, no key file var.
	if !strings.Contains(vars, `"ansible_password":"ssh-pa$$word"`) {
		t.Errorf("ansible_password missing:\n%s", vars)
	}
	if strings.Contains(vars, "ansible_ssh_private_key_file") {
		t.Errorf("key file var should be absent for password auth:\n%s", vars)
	}
}

// Key-file auth: ansible_ssh_private_key_file set, no ansible_password.
func TestBuildVarsLinuxKeyAuth(t *testing.T) {
	settings := &model.Settings{ControllerURL: "http://c:8080", NodeBinDir: "/opt/encode/bin"}
	req := linuxReq()
	req.SSHPassword = ""
	req.SSHKeyFile = "/home/deploy/.ssh/id_ed25519"
	vars := buildVars(settings, req, "code", true, true, nil, nil)
	if !strings.Contains(vars, `"ansible_ssh_private_key_file":"/home/deploy/.ssh/id_ed25519"`) {
		t.Errorf("key file var missing:\n%s", vars)
	}
	if strings.Contains(vars, "ansible_password") {
		t.Errorf("password var should be absent for key auth:\n%s", vars)
	}
}

// Linux buildVars with no SMB shares: no cifs vars, mount_cifs false.
func TestBuildVarsLinuxNoCIFS(t *testing.T) {
	settings := &model.Settings{
		ControllerURL: "http://controller:8080",
		NodeBinDir:    "/opt/encode/bin",
	}
	req := linuxReq()
	req.MountShares = false
	vars := buildVars(settings, req, "code", true, true, nil, nil)
	if !strings.Contains(vars, `"encode_mount_cifs":false`) {
		t.Errorf("cifs should be off without shares:\n%s", vars)
	}
	// No cifs credential VALUE should be populated (the key may exist as
	// an empty string, but no real password).
	if strings.Contains(vars, `encode_cifs_scripts_pass":"`) && !strings.Contains(vars, `encode_cifs_scripts_pass":""`) {
		t.Errorf("no cifs scripts password value should appear:\n%s", vars)
	}
}

// Linux buildVars with legacy NFS settings renders nfs server:/path export
// (same shape as the Windows path: NFS mounts come from legacy Settings).
func TestBuildVarsLinuxNFS(t *testing.T) {
	settings := &model.Settings{
		ControllerURL:  "http://controller:8080",
		NFSServer:      "nas01",
		ScriptsShare:   "/mnt/user/scripts",
		NodeBinDir:     "/opt/encode/bin",
		NodeScriptsDir: "/mnt/encodes/scripts",
		NodeReleaseDir: "/mnt/encodes/release",
	}
	req := linuxReq()
	req.MountShares = true
	vars := buildVars(settings, req, "code", true, true, nil, nil)
	if !strings.Contains(vars, `"encode_nfs_scripts_export":"nas01:/mnt/user/scripts"`) {
		t.Errorf("nfs scripts export missing:\n%s", vars)
	}
	if !strings.Contains(vars, `"encode_mount_nfs":true`) {
		t.Errorf("nfs should mount:\n%s", vars)
	}
}

// Linux buildVars with an S3 share produces no mount vars (agent-side).
func TestBuildVarsLinuxS3NoMount(t *testing.T) {
	settings := &model.Settings{
		ControllerURL: "http://controller:8080",
		NodeBinDir:    "/opt/encode/bin",
	}
	s3 := &model.Share{
		Name: "minio", Kind: model.ShareS3, Role: model.ShareRoleRelease,
		Endpoint: "http://minio:9000", Path: "releases", Enabled: true,
	}
	req := linuxReq()
	req.MountShares = true
	vars := buildVars(settings, req, "code", true, true, nil, s3)
	if !strings.Contains(vars, `"encode_mount_cifs":false`) {
		t.Errorf("no cifs mount for s3:\n%s", vars)
	}
	// S3 share owns the release role: no cifs release credential value.
	if strings.Contains(vars, `encode_cifs_release_pass":"`) && !strings.Contains(vars, `encode_cifs_release_pass":""`) {
		t.Errorf("s3 share must not render a cifs password value:\n%s", vars)
	}
}

// --- Redaction: cifs credentials must be scrubbed from the run log ---

func TestStreamCommandRedactsCIFSCredentials(t *testing.T) {
	st := &fakeProvStore{}
	// Simulate ansible echoing a cifs password into its output (a task
	// running `mount` could surface the password if the creds file path
	// were ever printed; defense in depth still scrubs the value).
	cmd := exec.Command("sh", "-c", "echo 'mounting with pass=c!f$pa$$'; echo done")
	done := make(chan error, 1)
	go func() {
		done <- streamCommand(context.Background(), context.Background(), st, 7, cmd, 1024*1024,
			[]string{"c!f$pa$$"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamCommand error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: streamCommand did not return")
	}
	logged := st.logs[7]
	if strings.Contains(logged, "c!f$pa$$") {
		t.Errorf("cifs password leaked into run log:\n%s", logged)
	}
	if !strings.Contains(logged, "[REDACTED]") {
		t.Errorf("redaction marker missing:\n%s", logged)
	}
}
