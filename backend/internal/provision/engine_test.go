package provision

import (
	"strings"
	"testing"

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
