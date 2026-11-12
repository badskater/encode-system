package offsite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeS3 mirrors the scanner/agent test pattern: an in-memory
// s3.ObjectStore keyed by bucket/key, recording uploads and deletes.
type fakeS3 struct {
	objects map[string][]byte // "bucket/key" -> content
	puts    []string
	deletes []string
	listErr error
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []model.S3Object
	for k, v := range f.objects {
		b, key, _ := strings.Cut(k, "/")
		if b == bucket && strings.HasPrefix(key, prefix) {
			out = append(out, model.S3Object{Key: key, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeS3) Get(ctx context.Context, bucket, key, dest string) error {
	b, ok := f.objects[bucket+"/"+key]
	if !ok {
		return os.ErrNotExist
	}
	return os.WriteFile(dest, b, 0o600)
}

func (f *fakeS3) Put(ctx context.Context, bucket, key, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f.objects[bucket+"/"+key] = b
	f.puts = append(f.puts, bucket+"/"+key)
	return nil
}

func (f *fakeS3) Delete(ctx context.Context, bucket, key string) error {
	delete(f.objects, bucket+"/"+key)
	f.deletes = append(f.deletes, bucket+"/"+key)
	return nil
}

// TestS3DestinationPaths: Put/List/Delete address bucket+prefix correctly
// and List strips the prefix back to plain names.
func TestS3DestinationPaths(t *testing.T) {
	f := newFakeS3()
	d := &S3Destination{Store: f, Bucket: "backups", Prefix: "encode/farm/"}
	dir := t.TempDir()
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, []byte("db-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Put(ctx, "snap.db", src); err != nil {
		t.Fatal(err)
	}
	if string(f.objects["backups/encode/farm/snap.db"]) != "db-bytes" {
		t.Fatalf("upload landed at the wrong key: %v", f.puts)
	}
	names, err := d.List(ctx)
	if err != nil || len(names) != 1 || names[0] != "snap.db" {
		t.Fatalf("List must strip the prefix: %v %v", names, err)
	}
	if err := d.Delete(ctx, "snap.db"); err != nil {
		t.Fatal(err)
	}
	if len(f.objects) != 0 {
		t.Fatalf("Delete must remove the prefixed key: %v", f.objects)
	}
}

// TestS3DestinationTraversalGuard: a hostile listing entry cannot escape
// the prefix via Put/Delete names.
func TestS3DestinationTraversalGuard(t *testing.T) {
	d := &S3Destination{Store: newFakeS3(), Bucket: "b", Prefix: "p/"}
	if err := d.Put(context.Background(), "../evil", "unused"); err == nil {
		t.Fatal("Put must reject traversal names")
	}
	if err := d.Delete(context.Background(), "../evil"); err == nil {
		t.Fatal("Delete must reject traversal names")
	}
}

// TestS3DestinationListError: listing failures propagate (Run turns them
// into PruneErr).
func TestS3DestinationListError(t *testing.T) {
	f := newFakeS3()
	f.listErr = errors.New("access denied")
	d := &S3Destination{Store: f, Bucket: "b"}
	if _, err := d.List(context.Background()); err == nil {
		t.Fatal("list error must propagate")
	}
}

// TestBuildS3: construction from settings succeeds without dialing (minio
// defers connection); an empty endpoint is rejected.
func TestBuildS3(t *testing.T) {
	cfg := model.BackupOffsiteS3{
		Enabled: true, Endpoint: "minio.example.com:9000", Bucket: "backups",
		Prefix: "encode/", AccessKey: "AK" + "TEST", SecretKey: "SK" + "TEST", UseTLS: true,
	}
	d, err := BuildS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d.Bucket != "backups" || d.Prefix != "encode/" {
		t.Fatalf("wrong destination config: %+v", d)
	}
	if _, err := BuildS3(model.BackupOffsiteS3{}); err == nil {
		t.Fatal("empty endpoint must be rejected")
	}
}

// TestBuildDestinationsS3: enabled S3 builds; disabled builds nothing;
// a bad S3 endpoint becomes an error Result, not a panic.
func TestBuildDestinationsS3(t *testing.T) {
	st := &model.Settings{}
	st.BackupOffsite.S3 = model.BackupOffsiteS3{
		Enabled: true, Endpoint: "minio.example.com:9000", Bucket: "b",
		AccessKey: "AK" + "TEST", SecretKey: "SK" + "TEST",
	}
	dests, errs := BuildDestinations(context.Background(), st, nil)
	if len(dests) != 1 || len(errs) != 0 || dests[0].Name() != "s3" {
		t.Fatalf("want 1 s3 destination: %v %v", dests, errs)
	}
	st.BackupOffsite.S3.Enabled = false
	dests, errs = BuildDestinations(context.Background(), st, nil)
	if len(dests) != 0 || len(errs) != 0 {
		t.Fatalf("disabled destination must not build: %v %v", dests, errs)
	}
	st.BackupOffsite.S3.Enabled = true
	st.BackupOffsite.S3.Endpoint = ""
	dests, errs = BuildDestinations(context.Background(), st, nil)
	if len(dests) != 0 || len(errs) != 1 || errs[0].Destination != "s3" || errs[0].Err == "" {
		t.Fatalf("build failure must be an error result: %v %v", dests, errs)
	}
}
