package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectStore is the narrow object-storage surface the job pipeline needs
// (list/get/put). Production uses minioClientStore (minio-go, S3 API —
// works against MinIO, Ceph RGW, AWS); tests use an in-memory fake.
type ObjectStore interface {
	List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error)
	Get(ctx context.Context, bucket, key, dest string) error
	Put(ctx context.Context, bucket, key, src string) error
}

// minioClientStore adapts a minio.Client to ObjectStore.
type minioClientStore struct{ c *minio.Client }

// newObjectStore builds the production store from a transfer spec.
// Endpoint is host[:port] without scheme (minio-go convention); UseTLS
// selects https.
func newObjectStore(t *model.S3Transfer) (ObjectStore, error) {
	c, err := minio.New(t.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(t.AccessKey, t.SecretKey, ""),
		Secure: t.UseTLS,
		Region: t.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &minioClientStore{c: c}, nil
}

func (m *minioClientStore) List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error) {
	var out []model.S3Object
	for obj := range m.c.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix: prefix, Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if obj.Key == "" || strings.HasSuffix(obj.Key, "/") {
			continue // directory markers carry no content
		}
		out = append(out, model.S3Object{Key: obj.Key, Size: obj.Size})
	}
	return out, nil
}

func (m *minioClientStore) Get(ctx context.Context, bucket, key, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return m.c.FGetObject(ctx, bucket, key, dest, minio.GetObjectOptions{})
}

func (m *minioClientStore) Put(ctx context.Context, bucket, key, src string) error {
	_, err := m.c.FPutObject(ctx, bucket, key, src, minio.PutObjectOptions{})
	return err
}

// expandS3Paths replaces the {{JOBDIR}} placeholder in every LocalDir of
// the transfer spec with the agent's real per-job directory. The
// controller renders placeholders because it never knows node-local paths.
func expandS3Paths(t *model.S3Transfer, jobDir string) {
	for i := range t.Downloads {
		t.Downloads[i].LocalDir = strings.ReplaceAll(t.Downloads[i].LocalDir, "{{JOBDIR}}", jobDir)
	}
	for i := range t.Uploads {
		t.Uploads[i].LocalDir = strings.ReplaceAll(t.Uploads[i].LocalDir, "{{JOBDIR}}", jobDir)
	}
}

// runS3Downloads pulls every object under each Download prefix into its
// local dir before the job script runs. Keys keep their path RELATIVE to
// the prefix (prefix "4k-test/Ep 02" + key "4k-test/Ep 02/src.mkv" →
// <LocalDir>/src.mkv), mirroring the mounted-share layout the rendered
// scripts expect.
func runS3Downloads(ctx context.Context, store ObjectStore, t *model.S3Transfer) error {
	for _, d := range t.Downloads {
		objs, err := store.List(ctx, d.Bucket, d.Prefix)
		if err != nil {
			return fmt.Errorf("list s3://%s/%s: %w", d.Bucket, d.Prefix, err)
		}
		if len(objs) == 0 {
			return fmt.Errorf("s3://%s/%s is empty — nothing to download", d.Bucket, d.Prefix)
		}
		if err := os.MkdirAll(d.LocalDir, 0o755); err != nil {
			return err
		}
		for _, o := range objs {
			rel := strings.TrimPrefix(o.Key, d.Prefix)
			rel = strings.TrimPrefix(rel, "/")
			if rel == "" {
				continue
			}
			dest := filepath.Join(d.LocalDir, filepath.FromSlash(rel))
			if err := store.Get(ctx, d.Bucket, o.Key, dest); err != nil {
				return fmt.Errorf("get s3://%s/%s: %w", d.Bucket, o.Key, err)
			}
		}
	}
	return nil
}

// runS3Uploads pushes every file under each Upload dir to its bucket
// prefix after a successful run, preserving relative paths (POSIX slashes
// in keys). Empty dirs are skipped — object stores have no directories.
func runS3Uploads(ctx context.Context, store ObjectStore, t *model.S3Transfer) error {
	for _, u := range t.Uploads {
		err := filepath.WalkDir(u.LocalDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(u.LocalDir, p)
			if err != nil {
				return err
			}
			key := strings.TrimSuffix(u.Prefix, "/") + "/" + filepath.ToSlash(rel)
			if err := store.Put(ctx, u.Bucket, key, p); err != nil {
				return fmt.Errorf("put s3://%s/%s: %w", u.Bucket, key, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
