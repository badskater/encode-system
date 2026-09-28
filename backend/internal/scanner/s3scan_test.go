package scanner

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeStore serves a flat key→(size,mtime) map like a real bucket listing.
type fakeS3Store struct {
	objects map[string]model.S3Object
	failErr error
}

func (f *fakeS3Store) List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	var out []model.S3Object
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}
func (f *fakeS3Store) Get(context.Context, string, string, string) error { return nil }
func (f *fakeS3Store) Put(context.Context, string, string, string) error { return nil }

func obj(key string, mtime time.Time) model.S3Object {
	return model.S3Object{Key: key, Size: 100, LastModified: mtime}
}

// TestScanS3FindsReadyEpisodes verifies the bucket scan mirrors the
// filesystem rules: depth-2 layout, source + script required, script
// priority, stability gate on LastModified.
func TestScanS3FindsReadyEpisodes(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	now := time.Now()

	tests := []struct {
		name    string
		objects map[string]model.S3Object
		want    []Candidate // expected candidates (sorted by EpisodeDir)
	}{
		{
			name: "ready episode is found",
			objects: map[string]model.S3Object{
				"show/Ep 01/src.m2ts": obj("show/Ep 01/src.m2ts", old),
				"show/Ep 01/2160.vpy": obj("show/Ep 01/2160.vpy", old),
			},
			want: []Candidate{{Series: "show", EpisodeDir: "show/Ep 01", ScriptType: "vpy", ScriptFile: "2160.vpy", SourceFile: "src.m2ts"}},
		},
		{
			name: "missing script is skipped",
			objects: map[string]model.S3Object{
				"show/Ep 01/src.m2ts": obj("show/Ep 01/src.m2ts", old),
			},
			want: nil,
		},
		{
			name: "missing source is skipped",
			objects: map[string]model.S3Object{
				"show/Ep 01/2160.vpy": obj("show/Ep 01/2160.vpy", old),
			},
			want: nil,
		},
		{
			name: "fresh source deferred by stability gate",
			objects: map[string]model.S3Object{
				"show/Ep 01/src.m2ts": obj("show/Ep 01/src.m2ts", now),
				"show/Ep 01/2160.vpy": obj("show/Ep 01/2160.vpy", old),
			},
			want: nil,
		},
		{
			name: "raw container beats mkv output",
			objects: map[string]model.S3Object{
				"show/Ep 01/show - 01 [1080p].mkv": obj("show/Ep 01/show - 01 [1080p].mkv", old),
				"show/Ep 01/src.m2ts":              obj("show/Ep 01/src.m2ts", old),
				"show/Ep 01/1080.avs":              obj("show/Ep 01/1080.avs", old),
			},
			want: []Candidate{{Series: "show", EpisodeDir: "show/Ep 01", ScriptType: "avs", ScriptFile: "1080.avs", SourceFile: "src.m2ts"}},
		},
		{
			name: "2160 script wins over 1080",
			objects: map[string]model.S3Object{
				"show/Ep 01/src.m2ts": obj("show/Ep 01/src.m2ts", old),
				"show/Ep 01/1080.avs": obj("show/Ep 01/1080.avs", old),
				"show/Ep 01/2160.vpy": obj("show/Ep 01/2160.vpy", old),
			},
			want: []Candidate{{Series: "show", EpisodeDir: "show/Ep 01", ScriptType: "vpy", ScriptFile: "2160.vpy", SourceFile: "src.m2ts"}},
		},
		{
			name: "deeper nesting ignored",
			objects: map[string]model.S3Object{
				"show/Ep 01/sub/src.m2ts": obj("show/Ep 01/sub/src.m2ts", old),
				"show/Ep 01/sub/2160.vpy": obj("show/Ep 01/sub/2160.vpy", old),
			},
			want: nil,
		},
		{
			name: "multiple episodes sorted",
			objects: map[string]model.S3Object{
				"show/Ep 02/src.m2ts": obj("show/Ep 02/src.m2ts", old),
				"show/Ep 02/2160.vpy": obj("show/Ep 02/2160.vpy", old),
				"show/Ep 01/src.m2ts": obj("show/Ep 01/src.m2ts", old),
				"show/Ep 01/2160.vpy": obj("show/Ep 01/2160.vpy", old),
			},
			want: []Candidate{
				{Series: "show", EpisodeDir: "show/Ep 01", ScriptType: "vpy", ScriptFile: "2160.vpy", SourceFile: "src.m2ts"},
				{Series: "show", EpisodeDir: "show/Ep 02", ScriptType: "vpy", ScriptFile: "2160.vpy", SourceFile: "src.m2ts"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeS3Store{objects: tt.objects}
			got, err := ScanS3(context.Background(), st, "bucket", 2*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d candidates %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("candidate[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestScanS3ListErrorPropagates: a bucket-listing failure must surface as
// an error (the loop logs it) rather than silently returning no candidates.
func TestScanS3ListErrorPropagates(t *testing.T) {
	st := &fakeS3Store{failErr: context.DeadlineExceeded}
	if _, err := ScanS3(context.Background(), st, "bucket", 0); err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestScanOnceS3TargetCreatesJobs verifies the loop path end to end with an
// s3 target: a ready bucket episode becomes a job (series auto-registered,
// dedupe honored) with no filesystem root involved.
func TestScanOnceS3TargetCreatesJobs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := &fakeStore{existing: map[string]bool{}, flow: &model.Flow{ID: 7, Name: "default-1080"}}
	old := time.Now().Add(-1 * time.Hour)
	bucket := &fakeS3Store{objects: map[string]model.S3Object{
		"show/Ep 01/src.m2ts": obj("show/Ep 01/src.m2ts", old),
		"show/Ep 01/2160.vpy": obj("show/Ep 01/2160.vpy", old),
		"show/Ep 02/src.m2ts": obj("show/Ep 02/src.m2ts", old), // no script: skipped
	}}

	scanOnce(context.Background(), log, st, Target{S3Bucket: "scripts", S3Store: bucket}, "default-1080")
	if len(st.created) != 1 {
		t.Fatalf("created %d jobs, want 1: %+v", len(st.created), st.created)
	}
	j := st.created[0]
	if j.EpisodeDir != "show/Ep 01" || j.ScriptFile != "2160.vpy" || j.ScriptType != "vpy" || j.FlowID != 7 {
		t.Fatalf("job = %+v", j)
	}

	// Second pass: dedupe keeps it at one job.
	st.existing["show/Ep 01"] = true
	scanOnce(context.Background(), log, st, Target{S3Bucket: "scripts", S3Store: bucket}, "default-1080")
	if len(st.created) != 1 {
		t.Fatalf("dedupe failed: created %d jobs, want 1", len(st.created))
	}
}
