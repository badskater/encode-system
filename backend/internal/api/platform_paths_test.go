package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
)

// setLinuxPathsSettings writes a settings row with the Windows node paths
// (required by validateSettings) plus the given Linux overrides. Used by the
// per-platform render tests so each starts from a known persisted row.
func setLinuxPathsSettings(t *testing.T, e *testEnv, linuxBin, linuxScripts, linuxRelease string) {
	t.Helper()
	ctx := ctxBg()
	st := e.server.defaultsSettings()
	st.LinuxBinDir = linuxBin
	st.LinuxScriptsDir = linuxScripts
	st.LinuxReleaseDir = linuxRelease
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
}

// assertBashVar finds the bash-rendered assignment line for a context var and
// checks it equals the expected single-quoted value. The bash dialect emits
// `VarName='value'` lines; we match the whole assignment so a prefix-only
// check cannot false-pass.
func assertBashVar(t *testing.T, script, label, want string) {
	t.Helper()
	wantLine := label + "=" + (flow.BashDialect{}).Quote(want)
	if !strings.Contains(script, wantLine) {
		t.Fatalf("%s not rendered correctly in script:\nwant line: %s\nscript:\n%s", label, wantLine, script)
	}
}

// TestRenderLinuxNodeUsesLinuxScriptsDir verifies that when a job renders for
// a Linux node AND the operator set linux_scripts_dir, the rendered bash
// script's ScriptsDir carries the Linux (POSIX) value instead of the Windows
// one. BinDir and ReleaseDir are checked too.
func TestRenderLinuxNodeUsesLinuxScriptsDir(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	setLinuxPathsSettings(t, e, "/opt/encode-bin", "/mnt/encode/scripts", "/mnt/encode/release")

	// A bash-renderable step template so the default-1080 dgindex step
	// (Windows-only) does not fail the render.
	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &model.StepTemplate{
		Key:   "plaf_noop",
		Label: "Platform Noop",
		PowerShell: "function Invoke-PlafNoop {\n" +
			"param($Job, $Params)\n" +
			"Write-Output \"ENCODE_STEP plaf 1.0\"\n" +
			"Write-Output \"ENCODE_JOB_DONE\"\n" +
			"}\n",
	})
	if err != nil {
		t.Fatalf("create step template: %v", err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "plaf-flow", Steps: []model.Step{{Type: model.StepType(tpl.Key)}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	payload, err := e.server.renderJob(ctx, job, model.PlatformLinux)
	if err != nil {
		t.Fatalf("render linux: %v", err)
	}
	assertBashVar(t, payload.Script, "BinDir", "/opt/encode-bin")
	assertBashVar(t, payload.Script, "ScriptsDir", "/mnt/encode/scripts")
	assertBashVar(t, payload.Script, "ReleaseDir", "/mnt/encode/release")
	// The Windows value must NOT appear in the rendered script.
	if strings.Contains(payload.Script, "C:\\bin") || strings.Contains(payload.Script, `C:\Encodes`) {
		t.Fatalf("linux render leaked a Windows path:\n%s", payload.Script)
	}
}

// TestRenderLinuxNodeEmptyLinuxFieldsFallsBackToWindows verifies back-compat:
// a Linux node with NO linux_* settings set falls back to the Windows values
// (the historical behavior — nothing changes until an operator sets them).
func TestRenderLinuxNodeEmptyLinuxFieldsFallsBackToWindows(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// Persist settings with empty linux_* fields (the default).
	setLinuxPathsSettings(t, e, "", "", "")

	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &model.StepTemplate{
		Key:   "fb_noop",
		Label: "Fallback Noop",
		PowerShell: "function Invoke-FbNoop {\n" +
			"param($Job, $Params)\n" +
			"Write-Output \"ENCODE_STEP fb 1.0\"\n" +
			"Write-Output \"ENCODE_JOB_DONE\"\n" +
			"}\n",
	})
	if err != nil {
		t.Fatalf("create step template: %v", err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "fb-flow", Steps: []model.Step{{Type: model.StepType(tpl.Key)}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	payload, err := e.server.renderJob(ctx, job, model.PlatformLinux)
	if err != nil {
		t.Fatalf("render linux: %v", err)
	}
	// Must fall back to the Windows values from the test config.
	assertBashVar(t, payload.Script, "BinDir", `C:\bin`)
	assertBashVar(t, payload.Script, "ScriptsDir", `C:\Encodes\scripts`)
	assertBashVar(t, payload.Script, "ReleaseDir", `C:\Encodes\ReleaseFolders`)
}

// TestRenderWindowsNodeAlwaysUsesWindowsPaths verifies a Windows node uses
// the Windows settings even when linux_* fields are set (the overrides are
// platform-scoped, not global).
func TestRenderWindowsNodeAlwaysUsesWindowsPaths(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	setLinuxPathsSettings(t, e, "/opt/encode-bin", "/mnt/encode/scripts", "/mnt/encode/release")

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	payload, err := e.server.renderJob(ctx, job, model.PlatformWindows)
	if err != nil {
		t.Fatalf("render windows: %v", err)
	}
	// Windows render carries $BinDir/$ScriptsDir as the Windows values.
	if !strings.Contains(payload.Script, `$BinDir     = 'C:\bin'`) {
		t.Fatalf("windows BinDir not the Windows value:\n%s", payload.Script)
	}
	if !strings.Contains(payload.Script, `$ScriptsDir = 'C:\Encodes\scripts'`) {
		t.Fatalf("windows ScriptsDir not the Windows value:\n%s", payload.Script)
	}
	// The Linux overrides must NOT appear.
	if strings.Contains(payload.Script, "/opt/encode-bin") || strings.Contains(payload.Script, "/mnt/encode") {
		t.Fatalf("windows render leaked a Linux path:\n%s", payload.Script)
	}
}

// TestRenderLinuxNodeS3StagingWinsOverLinuxScriptsDir verifies S3 staging
// precedence: when the scripts role is backed by an S3 share, the staging
// {{JOBDIR}} placeholder must win over BOTH the linux_scripts_dir AND the
// windows value — the per-platform override must not break S3 pulls.
func TestRenderLinuxNodeS3StagingWinsOverLinuxScriptsDir(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	setLinuxPathsSettings(t, e, "/opt/encode-bin", "/mnt/encode/scripts", "/mnt/encode/release")

	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "s3plaf", Steps: []model.Step{{Type: model.StepSourceRename}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateShare(ctx, &model.Share{
		Name: "src-s3", Kind: model.ShareS3, Role: model.ShareRoleScripts,
		Endpoint: "minio:9000", Path: "scripts-bucket",
		Username: "AKIA", Password: "secret", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateShare(ctx, &model.Share{
		Name: "rel-s3", Kind: model.ShareS3, Role: model.ShareRoleRelease,
		Endpoint: "minio:9000", Path: "release-bucket",
		Username: "AKIA", Password: "secret", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	job := &model.Job{Series: "4k-test", EpisodeDir: "4k-test/Ep 02", FlowID: fl.ID}
	payload, err := e.server.renderJob(ctx, job, model.PlatformLinux)
	if err != nil {
		t.Fatalf("render linux+s3: %v", err)
	}
	if payload.S3 == nil {
		t.Fatal("payload.S3 = nil, want transfer spec")
	}
	// The staging placeholder must win — neither the linux nor the windows
	// ScriptsDir may appear; only {{JOBDIR}}/scripts.
	if !strings.Contains(payload.Script, "{{JOBDIR}}/scripts") {
		t.Fatalf("S3 staging did not win over linux_scripts_dir:\n%s", payload.Script)
	}
	if strings.Contains(payload.Script, "/mnt/encode/scripts") || strings.Contains(payload.Script, `C:\Encodes\scripts`) {
		t.Fatalf("S3 render leaked a settings ScriptsDir (staging must win):\n%s", payload.Script)
	}
	// BinDir is NOT staged (no S3 bin role) so the linux override still applies.
	assertBashVar(t, payload.Script, "BinDir", "/opt/encode-bin")
}

// TestSettingsLinuxPathsRoundTrip verifies the three linux_* fields persist
// through SaveSettings/GetSettings (the JSON-blob single-row table) and come
// back empty by default for a fresh save that omits them.
func TestSettingsLinuxPathsRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	st := e.server.defaultsSettings()
	st.LinuxBinDir = "/opt/encode-bin"
	st.LinuxScriptsDir = "/mnt/encode/scripts"
	st.LinuxReleaseDir = "/mnt/encode/release"
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := e.server.Store.GetSettings(ctx)
	if err != nil || got == nil {
		t.Fatalf("GetSettings: %v %v", got, err)
	}
	if got.LinuxBinDir != "/opt/encode-bin" || got.LinuxScriptsDir != "/mnt/encode/scripts" || got.LinuxReleaseDir != "/mnt/encode/release" {
		t.Fatalf("linux_* paths not persisted: %+v", got)
	}

	// Clear them back to empty — must round-trip as empty.
	st.LinuxBinDir = ""
	st.LinuxScriptsDir = ""
	st.LinuxReleaseDir = ""
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, _ = e.server.Store.GetSettings(ctx)
	if got.LinuxBinDir != "" || got.LinuxScriptsDir != "" || got.LinuxReleaseDir != "" {
		t.Fatalf("cleared linux_* paths not empty: %+v", got)
	}
}

// TestSettingsLinuxPathsAcceptedByAPI verifies the PUT /api/settings endpoint
// accepts and round-trips the linux_* fields (no validation rejects them —
// they are optional POSIX paths with the same validation class as the Windows
// node dirs, which is none for path shape).
func TestSettingsLinuxPathsAcceptedByAPI(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// GET current settings (env defaults).
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d %s", resp.StatusCode, body)
	}
	var st model.Settings
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	st.LinuxBinDir = "/opt/encode-bin"
	st.LinuxScriptsDir = "/mnt/encode/scripts"
	st.LinuxReleaseDir = "/mnt/encode/release"

	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != 200 {
		t.Fatalf("put settings with linux paths: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings after save: %d %s", resp.StatusCode, body)
	}
	var st2 model.Settings
	if err := json.Unmarshal(body, &st2); err != nil {
		t.Fatal(err)
	}
	if st2.LinuxBinDir != "/opt/encode-bin" || st2.LinuxScriptsDir != "/mnt/encode/scripts" || st2.LinuxReleaseDir != "/mnt/encode/release" {
		t.Fatalf("linux paths not round-tripped by API: %+v", st2)
	}
}
