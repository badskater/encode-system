package provision

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestBuildVarsSMBShares verifies that an SMB scripts+release share pair
// renders UNC/user/pass vars and encode_mount_smb, with NFS off.
func TestBuildVarsSMBShares(t *testing.T) {
	settings := &model.Settings{
		ControllerURL:  "http://c:8080",
		NodeBinDir:     `C:\bin`,
		NodeScriptsDir: `C:\Encodes\scripts`,
		NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
	}
	scripts := &model.Share{
		Name: "smb-scripts", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "unraid01", Path: "encodes", Username: "encode", Password: "s3cret",
		Enabled: true,
	}
	release := &model.Share{
		Name: "smb-release", Kind: model.ShareSMB, Role: model.ShareRoleRelease,
		Server: "unraid01", Path: "ReleaseFolders", Username: "encode", Password: "s3cret",
		Enabled: true,
	}
	req := Request{Host: "h", WinRMUser: "u", WinRMPassword: "p", NodeName: "n", MountShares: true}
	vars := buildVars(settings, req, "code", true, true, scripts, release)

	for _, want := range []string{
		`encode_smb_scripts_unc: '\\unraid01\encodes'`,
		`encode_smb_release_unc: '\\unraid01\ReleaseFolders'`,
		`encode_smb_scripts_user: 'encode'`,
		`encode_smb_scripts_pass: 's3cret'`,
		"encode_mount_smb: true",
		"encode_mount_nfs: false",
	} {
		if !strings.Contains(vars, want) {
			t.Errorf("vars missing %q\ngot:\n%s", want, vars)
		}
	}
}

// TestBuildVarsSharesPrecedence: an enabled SMB share wins over legacy NFS
// settings for the same role; an S3 share produces no mount vars (the agent
// pulls per job).
func TestBuildVarsSharesPrecedence(t *testing.T) {
	settings := &model.Settings{
		ControllerURL: "http://c:8080", NFSServer: "unraid01",
		ScriptsShare: "/mnt/user/scripts", ReleaseShare: "/mnt/user/ReleaseFolders",
		NodeBinDir: `C:\bin`,
	}
	smbScripts := &model.Share{
		Name: "s", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "nas", Path: "scripts", Username: "u", Password: "p", Enabled: true,
	}
	// Release stays on legacy NFS (no share row) — mixed transports.
	req := Request{Host: "h", WinRMUser: "u", WinRMPassword: "p", NodeName: "n", MountShares: true}
	vars := buildVars(settings, req, "code", false, false, smbScripts, nil)

	if !strings.Contains(vars, "encode_mount_smb: true") {
		t.Errorf("smb should mount:\n%s", vars)
	}
	if !strings.Contains(vars, "encode_nfs_release_export: 'unraid01:/mnt/user/ReleaseFolders'") {
		t.Errorf("legacy nfs release should remain:\n%s", vars)
	}
	if !strings.Contains(vars, "encode_mount_nfs: true") {
		t.Errorf("nfs should mount for the release role:\n%s", vars)
	}
	// NFS scripts export must be EMPTY — the SMB share took that role.
	if !strings.Contains(vars, "encode_nfs_scripts_export: ''") {
		t.Errorf("nfs scripts export should be empty when an smb share owns the role:\n%s", vars)
	}
}

// TestBuildVarsS3ShareNoMount: an s3 share for a role yields no mount vars
// for that role (agent-side transfer), but also no NFS fallback.
func TestBuildVarsS3ShareNoMount(t *testing.T) {
	settings := &model.Settings{
		ControllerURL: "http://c:8080", NFSServer: "unraid01",
		ScriptsShare: "/mnt/user/scripts", NodeBinDir: `C:\bin`,
	}
	s3Release := &model.Share{
		Name: "minio", Kind: model.ShareS3, Role: model.ShareRoleRelease,
		Endpoint: "http://minio:9000", Path: "releases", Enabled: true,
	}
	req := Request{Host: "h", WinRMUser: "u", WinRMPassword: "p", NodeName: "n", MountShares: true}
	vars := buildVars(settings, req, "code", false, false, nil, s3Release)

	if !strings.Contains(vars, "encode_mount_smb: false") {
		t.Errorf("no smb mount expected:\n%s", vars)
	}
	// The s3 share owns the release role: legacy NFS release must not mount.
	if !strings.Contains(vars, "encode_nfs_release_export: ''") {
		t.Errorf("nfs release should be empty when s3 owns the role:\n%s", vars)
	}
}
