package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// S3 parity for series script templates: reserved _templates prefix,
// source-only discovery, and bucket-side materialization.

// fakeS3IOStore is a file-backed ObjectStore: Get/Put move real bytes so
// materialization can be verified end-to-end against the bucket map.
type fakeS3IOStore struct {
	objects map[string]model.S3Object
	content map[string][]byte
}

func (f *fakeS3IOStore) List(_ context.Context, _, prefix string) ([]model.S3Object, error) {
	var out []model.S3Object
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeS3IOStore) Get(_ context.Context, _, key, dest string) error {
	body, ok := f.content[key]
	if !ok {
		return os.ErrNotExist
	}
	return os.WriteFile(dest, body, 0o644)
}

func (f *fakeS3IOStore) Put(_ context.Context, _, key, src string) error {
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if f.content == nil {
		f.content = map[string][]byte{}
	}
	f.content[key] = body
	f.objects[key] = model.S3Object{Key: key, Size: int64(len(body)), LastModified: time.Now()}
	return nil
}

func (f *fakeS3IOStore) Delete(_ context.Context, _, key string) error {
	delete(f.objects, key)
	delete(f.content, key)
	return nil
}

func TestScanS3SkipsTemplatesPrefix(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	store := &fakeS3Store{objects: map[string]model.S3Object{
		"_templates/S/1080.vpy": obj("_templates/S/1080.vpy", old),
		"S/Ep 01/src.m2ts":      obj("S/Ep 01/src.m2ts", old),
		"S/Ep 01/1080.vpy":      obj("S/Ep 01/1080.vpy", old),
	}}
	cands, err := ScanS3(context.Background(), store, "bucket", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Series != "S" {
		t.Fatalf("_templates must never scan as a series: %+v", cands)
	}
}

func TestScanS3SourceOnly(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	store := &fakeS3Store{objects: map[string]model.S3Object{
		"S/Ep 01/src.m2ts": obj("S/Ep 01/src.m2ts", old), // scriptless -> wanted
		"S/Ep 02/src.m2ts": obj("S/Ep 02/src.m2ts", old),
		"S/Ep 02/1080.vpy": obj("S/Ep 02/1080.vpy", old), // has script -> excluded
	}}
	cands, err := ScanS3SourceOnly(context.Background(), store, "bucket", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].EpisodeDir != "S/Ep 01" || cands[0].ScriptFile != "" {
		t.Fatalf("want only the scriptless episode: %+v", cands)
	}
}

func TestMaterializeScriptS3RoundTrip(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	store := &fakeS3IOStore{
		objects: map[string]model.S3Object{
			"S/Ep 01/src.m2ts":      obj("S/Ep 01/src.m2ts", old),
			"_templates/S/1080.vpy": obj("_templates/S/1080.vpy", old),
		},
		content: map[string][]byte{
			"_templates/S/1080.vpy": []byte("# {{series}} {{episode}} {{source}}\n"),
		},
	}
	sr := &model.Series{Name: "S", ScriptType: "vpy", ScriptTemplate: "1080.vpy"}
	c := Candidate{Series: "S", EpisodeDir: "S/Ep 01", SourceFile: "src.m2ts"}
	got, err := MaterializeScriptS3(context.Background(), store, "bucket", sr, c)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScriptFile != "1080.vpy" || got.ScriptType != "vpy" {
		t.Fatalf("candidate not updated: %+v", got)
	}
	body := string(store.content["S/Ep 01/1080.vpy"])
	if body != "# S Ep 01 src.m2ts\n" {
		t.Fatalf("rendered bucket object wrong: %q", body)
	}
	// Second call must refuse: the episode now carries a script.
	if _, err := MaterializeScriptS3(context.Background(), store, "bucket", sr, c); err == nil {
		t.Fatal("must refuse overwrite of an existing episode script")
	}
	// No temp dirs leaked into the system temp root by this call path.
	entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "encode-materialize-*"))
	if len(entries) != 0 {
		t.Fatalf("temp dirs leaked: %v", entries)
	}
}

func TestMaterializeScriptS3MissingTemplate(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	store := &fakeS3IOStore{
		objects: map[string]model.S3Object{"S/Ep 01/src.m2ts": obj("S/Ep 01/src.m2ts", old)},
		content: map[string][]byte{},
	}
	sr := &model.Series{Name: "S", ScriptType: "vpy", ScriptTemplate: "gone.vpy"}
	_, err := MaterializeScriptS3(context.Background(), store, "bucket", sr, Candidate{Series: "S", EpisodeDir: "S/Ep 01", SourceFile: "src.m2ts"})
	if err == nil {
		t.Fatal("missing template must fail closed")
	}
}

// Loop end-to-end on the S3 target: assignment + scriptless episode ->
// materialized bucket object + job.
func TestScanOnceS3MaterializesSeriesTemplate(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	store := &fakeS3IOStore{
		objects: map[string]model.S3Object{
			"S/Ep 01/src.m2ts":      obj("S/Ep 01/src.m2ts", old),
			"_templates/S/1080.vpy": obj("_templates/S/1080.vpy", old),
		},
		content: map[string][]byte{
			"_templates/S/1080.vpy": []byte("# {{episode}}\n"),
		},
	}
	st := &fakeStore{
		existing: map[string]bool{},
		flow:     &model.Flow{ID: 9, Name: "default-1080"},
		series: map[string]*model.Series{
			"S": {Name: "S", Enabled: true, ScriptType: "vpy", ScriptTemplate: "1080.vpy"},
		},
	}
	scanOnce(context.Background(), discardLog(), st, Target{S3Bucket: "bucket", S3Store: store}, "default-1080")
	if len(st.created) != 1 || st.created[0].ScriptFile != "1080.vpy" {
		t.Fatalf("want 1 job with the materialized script, got %+v", st.created)
	}
	if string(store.content["S/Ep 01/1080.vpy"]) != "# Ep 01\n" {
		t.Fatalf("bucket script wrong: %q", store.content["S/Ep 01/1080.vpy"])
	}
}
