package scanner

import (
	"context"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// BDMV (Blu-ray disc layout) episodes in the S3 scanner: playlist/stream
// objects live DEEPER than the depth-2 flat files, so the grouping must
// capture them instead of dropping them. Same rules as the filesystem
// scan: flat source wins, playlist+stream both required, stability gate on
// the newest BDMV object.

func bdmvObjects(ep string, old time.Time) map[string]model.S3Object {
	return map[string]model.S3Object{
		ep + "/BDMV/PLAYLIST/00000.mpls": obj(ep+"/BDMV/PLAYLIST/00000.mpls", old),
		ep + "/BDMV/STREAM/00000.m2ts":   obj(ep+"/BDMV/STREAM/00000.m2ts", old),
		ep + "/1080.vpy":                 obj(ep+"/1080.vpy", old),
	}
}

func TestScanS3BDMVEpisode(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	store := &fakeS3Store{objects: bdmvObjects("show/Ep 01", old)}

	cands, err := ScanS3(context.Background(), store, "bucket", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Series != "show" || c.EpisodeDir != "show/Ep 01" || c.ScriptFile != "1080.vpy" || c.ScriptType != "vpy" {
		t.Errorf("wrong identity: %+v", c)
	}
	if c.SourceFile != "BDMV/PLAYLIST/00000.mpls" {
		t.Errorf("SourceFile must be the playlist rel path, got %q", c.SourceFile)
	}
}

func TestScanS3FlatSourceBeatsBDMV(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	objs := bdmvObjects("show/Ep 01", old)
	objs["show/Ep 01/src.m2ts"] = obj("show/Ep 01/src.m2ts", old)
	store := &fakeS3Store{objects: objs}

	cands, err := ScanS3(context.Background(), store, "bucket", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].SourceFile != "src.m2ts" {
		t.Fatalf("flat source must outrank BDMV: %+v", cands)
	}
}

func TestScanS3BDMVNeedsStream(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	objs := bdmvObjects("show/Ep 01", old)
	delete(objs, "show/Ep 01/BDMV/STREAM/00000.m2ts")
	store := &fakeS3Store{objects: objs}

	cands, _ := ScanS3(context.Background(), store, "bucket", 0)
	if len(cands) != 0 {
		t.Fatalf("BDMV without streams must be skipped: %+v", cands)
	}
}

func TestScanS3BDMVNeedsScript(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	objs := bdmvObjects("show/Ep 01", old)
	delete(objs, "show/Ep 01/1080.vpy")
	store := &fakeS3Store{objects: objs}

	cands, _ := ScanS3(context.Background(), store, "bucket", 0)
	if len(cands) != 0 {
		t.Fatalf("BDMV without script must be skipped: %+v", cands)
	}
}

func TestScanS3BDMVStabilityGate(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	now := time.Now()
	objs := bdmvObjects("show/Ep 01", old)
	// The big stream file is still uploading.
	objs["show/Ep 01/BDMV/STREAM/00000.m2ts"] = obj("show/Ep 01/BDMV/STREAM/00000.m2ts", now)
	store := &fakeS3Store{objects: objs}

	cands, _ := ScanS3(context.Background(), store, "bucket", 30*time.Minute)
	if len(cands) != 0 {
		t.Fatalf("fresh BDMV stream must defer the episode: %+v", cands)
	}
}

// An episode whose ONLY depth-2 file is the script (everything else is
// BDMV subtree) must still be grouped and found.
func TestScanS3BDMVOnlyDeepKeys(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	ep := "show/Ep 02"
	objs := map[string]model.S3Object{
		ep + "/BDMV/PLAYLIST/00001.mpls": obj(ep+"/BDMV/PLAYLIST/00001.mpls", old),
		ep + "/BDMV/PLAYLIST/00002.mpls": obj(ep+"/BDMV/PLAYLIST/00002.mpls", old),
		ep + "/BDMV/STREAM/00001.m2ts":   obj(ep+"/BDMV/STREAM/00001.m2ts", old),
		ep + "/BDMV/STREAM/00002.m2ts":   obj(ep+"/BDMV/STREAM/00002.m2ts", old),
		ep + "/BDMV/CLIPINF/00001.clpi":  obj(ep+"/BDMV/CLIPINF/00001.clpi", old),
		ep + "/2160.vpy":                 obj(ep+"/2160.vpy", old),
	}
	store := &fakeS3Store{objects: objs}

	cands, err := ScanS3(context.Background(), store, "bucket", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(cands), cands)
	}
	if cands[0].SourceFile != "BDMV/PLAYLIST/00001.mpls" {
		t.Errorf("want first sorted playlist, got %q", cands[0].SourceFile)
	}
}
