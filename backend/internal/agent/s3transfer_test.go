package agent

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeObjectStore records transfers and serves in-memory objects.
type fakeObjectStore struct {
	objects map[string][]byte // "bucket/key"
	dl      []string          // keys downloaded
	ul      map[string][]byte // "bucket/key" -> bytes uploaded
	failGet bool
	failPut bool
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}, ul: map[string][]byte{}}
}

func (f *fakeObjectStore) List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error) {
	var out []model.S3Object
	for k, v := range f.objects {
		b, key := splitBucketKey(k)
		if b == bucket && (prefix == "" || len(key) >= len(prefix) && key[:len(prefix)] == prefix) {
			out = append(out, model.S3Object{Key: key, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func splitBucketKey(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func (f *fakeObjectStore) Get(ctx context.Context, bucket, key, dest string) error {
	if f.failGet {
		return os.ErrPermission
	}
	b, ok := f.objects[bucket+"/"+key]
	if !ok {
		return os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f.dl = append(f.dl, key)
	return os.WriteFile(dest, b, 0o644)
}

func (f *fakeObjectStore) Put(ctx context.Context, bucket, key string, src string) error {
	if f.failPut {
		return os.ErrPermission
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f.ul[bucket+"/"+key] = b
	return nil
}

func TestS3DownloadPrefix(t *testing.T) {
	dir := t.TempDir()
	f := newFakeObjectStore()
	f.objects["scripts/4k-test/Ep 02/src.mkv"] = []byte("video")
	f.objects["scripts/4k-test/Ep 02/2160.avs"] = []byte("script")
	f.objects["scripts/other/Ep 01/src.mkv"] = []byte("nope")

	tr := &model.S3Transfer{Downloads: []model.S3Download{{
		Bucket: "scripts", Prefix: "4k-test/Ep 02",
		LocalDir: filepath.Join(dir, "ep02"),
	}}}
	if err := runS3Downloads(context.Background(), f, tr); err != nil {
		t.Fatal(err)
	}
	if len(f.dl) != 2 {
		t.Fatalf("downloaded %v, want the 2 prefix objects", f.dl)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ep02", "src.mkv"))
	if err != nil || string(b) != "video" {
		t.Fatalf("src.mkv = %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ep02", "2160.avs")); err != nil {
		t.Fatal("script not downloaded")
	}
}

func TestS3DownloadFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	f := newFakeObjectStore()
	f.objects["b/p/f.bin"] = []byte("x")
	f.failGet = true
	tr := &model.S3Transfer{Downloads: []model.S3Download{{Bucket: "b", Prefix: "p", LocalDir: dir}}}
	if err := runS3Downloads(context.Background(), f, tr); err == nil {
		t.Fatal("want error when Get fails")
	}
}

func TestS3UploadDir(t *testing.T) {
	dir := t.TempDir()
	// local tree: out/release.mkv + out/sub/extra.txt
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "release.mkv"), []byte("mkv"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "extra.txt"), []byte("txt"), 0o644)

	f := newFakeObjectStore()
	tr := &model.S3Transfer{Uploads: []model.S3Upload{{
		Bucket: "releases", Prefix: "4k-test/Ep 02", LocalDir: dir,
	}}}
	if err := runS3Uploads(context.Background(), f, tr); err != nil {
		t.Fatal(err)
	}
	want := []string{"releases/4k-test/Ep 02/release.mkv", "releases/4k-test/Ep 02/sub/extra.txt"}
	got := make([]string, 0, len(f.ul))
	for k := range f.ul {
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("uploaded %v, want %v", got, want)
	}
}

func TestS3UploadFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.mkv"), []byte("x"), 0o644)
	f := newFakeObjectStore()
	f.failPut = true
	tr := &model.S3Transfer{Uploads: []model.S3Upload{{Bucket: "b", Prefix: "p", LocalDir: dir}}}
	if err := runS3Uploads(context.Background(), f, tr); err == nil {
		t.Fatal("want error when Put fails")
	}
}
