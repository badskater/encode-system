package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Settings ----------

func TestGetSettingsReturnsEnvDefaults(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d %s", resp.StatusCode, body)
	}
	var st model.Settings
	mustUnmarshal(t, body, &st)
	if st.NodeBinDir != `C:\bin` {
		t.Errorf("default node_bin_dir = %q", st.NodeBinDir)
	}
	if st.TasksBeforeReboot != 10 {
		t.Errorf("default tasks_before_reboot = %d", st.TasksBeforeReboot)
	}
	if st.ScanIntervalSeconds != 30 {
		t.Errorf("default scan_interval = %d", st.ScanIntervalSeconds)
	}
}

func TestUpdateSettingsPersistsAndRoundTrips(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Read defaults, modify the remote path mapping, save.
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	var st model.Settings
	mustUnmarshal(t, body, &st)
	st.NodeScriptsDir = `D:\Anime\scripts`
	st.ScanIntervalSeconds = 60

	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != 200 {
		t.Fatalf("put settings: %d %s", resp.StatusCode, body)
	}

	// Fresh GET must return the saved values.
	resp, body = doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	var st2 model.Settings
	mustUnmarshal(t, body, &st2)
	if st2.NodeScriptsDir != `D:\Anime\scripts` {
		t.Errorf("saved node_scripts_dir = %q", st2.NodeScriptsDir)
	}
	if st2.ScanIntervalSeconds != 60 {
		t.Errorf("saved scan_interval = %d", st2.ScanIntervalSeconds)
	}
}

func TestUpdateSettingsRejectsBadPaths(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	var st model.Settings
	mustUnmarshal(t, body, &st)

	cases := []struct {
		name   string
		mutate func(*model.Settings)
	}{
		{"unix path for node dir", func(s *model.Settings) { s.NodeBinDir = "/usr/bin" }},
		{"relative node dir", func(s *model.Settings) { s.NodeScriptsDir = "scripts" }},
		{"windows path for controller root", func(s *model.Settings) { s.ScriptsRoot = `C:\data` }},
		{"empty node bin dir", func(s *model.Settings) { s.NodeBinDir = "" }},
		{"scan interval too low", func(s *model.Settings) { s.ScanIntervalSeconds = 1 }},
		{"reboot threshold zero", func(s *model.Settings) { s.TasksBeforeReboot = 0 }},
		{"empty group", func(s *model.Settings) { s.Group = "" }},
	}
	for _, tc := range cases {
		cp := st
		cp.ScanIntervalSeconds = st.ScanIntervalSeconds
		tc.mutate(&cp)
		resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, cp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d (%s)", tc.name, resp.StatusCode, body)
		}
	}
}

func TestUpdateSettingsRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/settings", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without session, got %d", resp.StatusCode)
	}
}

// ---------- Publishing ----------

// mkZip builds a zip in memory from name→content pairs.
func mkZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// publishMultipart posts version+file to path with the admin token.
func publishMultipart(t *testing.T, url, path, version string, fileName string, content []byte) (*http.Response, []byte) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("version", version); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, err := http.NewRequest("POST", url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp, rb
}

func TestPublishBinHappyPath(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	z := mkZip(t, map[string][]byte{
		"x265_x64.exe": []byte("fake-x265"),
		"sub/tool.exe": []byte("fake-tool"),
	})
	resp, body := publishMultipart(t, ts.URL, "/api/updates/bin", "1", "bin.zip", z)
	if resp.StatusCode != 200 {
		t.Fatalf("publish bin: %d %s", resp.StatusCode, body)
	}
	var m model.UpdateManifest
	mustUnmarshal(t, body, &m)
	if m.BinVersion != 1 || m.BinSHA256 == "" || m.BinSize != int64(len(z)) {
		t.Fatalf("manifest wrong: %+v", m)
	}

	// Version must strictly increase.
	resp, _ = publishMultipart(t, ts.URL, "/api/updates/bin", "1", "bin.zip", z)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-publish same version: want 409, got %d", resp.StatusCode)
	}
}

func TestPublishBinRejectsTraversal(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	for _, evil := range []string{"../../etc/passwd", `..\..\windows\system32\x`, "/abs/path.exe"} {
		z := mkZip(t, map[string][]byte{evil: []byte("payload")})
		resp, body := publishMultipart(t, ts.URL, "/api/updates/bin", "1", "bin.zip", z)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("entry %q: want 400, got %d (%s)", evil, resp.StatusCode, body)
		}
	}
}

func TestPublishBinRejectsGarbage(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := publishMultipart(t, ts.URL, "/api/updates/bin", "1", "bin.zip", []byte("not a zip at all"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for non-zip, got %d", resp.StatusCode)
	}
}

func TestPublishAgentRequiresVersion(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Missing version field.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "agent.exe")
	fw.Write([]byte("binary"))
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/updates/agent", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 without version, got %d", resp.StatusCode)
	}

	// With version: publishes and the manifest reflects it.
	resp, rb := publishMultipart(t, ts.URL, "/api/updates/agent", "9.9.9", "agent.exe", []byte("binary"))
	if resp.StatusCode != 200 {
		t.Fatalf("publish agent: %d %s", resp.StatusCode, rb)
	}
	var m model.UpdateManifest
	mustUnmarshal(t, rb, &m)
	if m.AgentVersion != "9.9.9" || m.AgentSHA256 == "" {
		t.Fatalf("agent manifest wrong: %+v", m)
	}
}

func TestPublishLibMonotonic(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := publishMultipart(t, ts.URL, "/api/updates/lib", "5", "EncodeLib.ps1", []byte("Write-Output hi"))
	if resp.StatusCode != 200 {
		t.Fatalf("publish lib: %d %s", resp.StatusCode, body)
	}
	// Lower or equal version rejected.
	resp, _ = publishMultipart(t, ts.URL, "/api/updates/lib", "3", "EncodeLib.ps1", []byte("old"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("lower version: want 409, got %d", resp.StatusCode)
	}
	resp, _ = publishMultipart(t, ts.URL, "/api/updates/lib", "5", "EncodeLib.ps1", []byte("same"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("equal version: want 409, got %d", resp.StatusCode)
	}
}

func TestAdminManifestEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/updates/manifest", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d %s", resp.StatusCode, body)
	}
	var m model.UpdateManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
}
