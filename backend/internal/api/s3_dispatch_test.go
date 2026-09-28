package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestRenderJobS3Staging verifies that when the scripts/release roles are
// backed by s3 shares, the rendered payload carries:
//   - ScriptsDir/ReleaseDir vars pointing into the per-job staging dir
//     ({{JOBDIR}} placeholder — the agent expands it, the controller never
//     knows node paths)
//   - an S3 transfer spec downloading the episode prefix and uploading the
//     release dir to the release bucket.
func TestRenderJobS3Staging(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// Minimal flow: one exec step referencing the dirs so we can assert
	// the rendered script uses staging paths.
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:  "s3flow",
		Steps: []model.Step{{Type: model.StepSourceRename}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// s3 shares own both roles.
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
	payload, err := e.server.renderJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.S3 == nil {
		t.Fatal("payload.S3 = nil, want transfer spec")
	}
	if payload.S3.Endpoint != "minio:9000" || payload.S3.AccessKey != "AKIA" || payload.S3.SecretKey != "secret" {
		t.Fatalf("s3 creds/endpoint = %+v", payload.S3)
	}
	if len(payload.S3.Downloads) != 1 {
		t.Fatalf("downloads = %+v, want 1", payload.S3.Downloads)
	}
	d := payload.S3.Downloads[0]
	if d.Bucket != "scripts-bucket" || d.Prefix != "4k-test/Ep 02" {
		t.Fatalf("download = %+v", d)
	}
	// The rendered script computes $EpisodeDir = Join-Path $ScriptsDir
	// <episode_dir>, and downloads strip the bucket prefix — so sources
	// must land under scripts/<episode_dir>, not the scripts root.
	if d.LocalDir != "{{JOBDIR}}/scripts/4k-test/Ep 02" {
		t.Fatalf("download LocalDir = %q, want {{JOBDIR}}/scripts/4k-test/Ep 02", d.LocalDir)
	}
	if len(payload.S3.Uploads) != 1 {
		t.Fatalf("uploads = %+v, want 1", payload.S3.Uploads)
	}
	u := payload.S3.Uploads[0]
	if u.Bucket != "release-bucket" || u.Prefix != "4k-test/Ep 02" {
		t.Fatalf("upload = %+v", u)
	}
	// Rendered script points at staging dirs.
	if !strings.Contains(payload.Script, "{{JOBDIR}}") {
		t.Fatalf("script does not reference the staging placeholder:\n%s", payload.Script)
	}
}

// TestRenderJobMountSharesUnchanged verifies mount-backed shares (smb/nfs)
// leave the payload exactly as before: settings dirs, no S3 spec.
func TestRenderJobMountSharesUnchanged(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:  "smbflow",
		Steps: []model.Step{{Type: model.StepSourceRename}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateShare(ctx, &model.Share{
		Name: "smb-scripts", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "nas", Path: "scripts", Username: "u", Password: "p", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	job := &model.Job{Series: "s", EpisodeDir: "s/Ep 01", FlowID: fl.ID}
	payload, err := e.server.renderJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.S3 != nil {
		t.Fatalf("mount-backed job must not carry an S3 spec: %+v", payload.S3)
	}
	// Script still uses the settings node dirs.
	if strings.Contains(payload.Script, "{{JOBDIR}}") {
		t.Fatalf("mount-backed script must not use staging:\n%s", payload.Script)
	}
	if _, err := json.Marshal(payload); err != nil {
		t.Fatal(err)
	}
}
