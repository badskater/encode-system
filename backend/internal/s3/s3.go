// Package s3 provides the narrow object-storage surface used by both the
// agent (per-job download/upload staging) and the controller scanner
// (episode discovery by bucket listing). Production implementation is
// minio-go, which speaks plain S3: MinIO, Ceph RGW and AWS all work.
package s3

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectStore is the object-storage surface the pipeline needs. Kept
// minimal so both consumers unit-test against in-memory fakes.
type ObjectStore interface {
	List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error)
	Get(ctx context.Context, bucket, key, dest string) error
	Put(ctx context.Context, bucket, key, src string) error
}

// EndpointFromShare normalizes a share's connection fields to the
// host[:port] form minio-go wants (no scheme): a full URL in Endpoint
// wins, else Server[:Port].
func EndpointFromShare(sh *model.Share) string {
	ep := strings.TrimSpace(sh.Endpoint)
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	ep = strings.TrimSuffix(ep, "/")
	if ep != "" {
		return ep
	}
	if sh.Port > 0 {
		return fmt.Sprintf("%s:%d", sh.Server, sh.Port)
	}
	return sh.Server
}

// client adapts a minio.Client to ObjectStore.
type client struct{ c *minio.Client }

// New builds a production store from a transfer spec. Endpoint is
// host[:port] without scheme (minio-go convention); UseTLS selects https.
func New(t *model.S3Transfer) (ObjectStore, error) {
	if strings.TrimSpace(t.Endpoint) == "" {
		return nil, fmt.Errorf("s3: empty endpoint")
	}
	c, err := minio.New(t.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(t.AccessKey, t.SecretKey, ""),
		Secure: t.UseTLS,
		Region: t.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &client{c: c}, nil
}

func (m *client) List(ctx context.Context, bucket, prefix string) ([]model.S3Object, error) {
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
		out = append(out, model.S3Object{
			Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified,
		})
	}
	return out, nil
}

func (m *client) Get(ctx context.Context, bucket, key, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return m.c.FGetObject(ctx, bucket, key, dest, minio.GetObjectOptions{})
}

func (m *client) Put(ctx context.Context, bucket, key, src string) error {
	_, err := m.c.FPutObject(ctx, bucket, key, src, minio.PutObjectOptions{})
	return err
}
