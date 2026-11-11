package offsite

import (
	"context"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/s3"
)

// S3Destination adapts the shared internal/s3.ObjectStore (minio-go) to the
// offsite Destination surface, baking in the bucket + key prefix. Works
// against any plain-S3 endpoint: MinIO, Ceph RGW, AWS.
type S3Destination struct {
	Store  s3.ObjectStore
	Bucket string
	Prefix string // normalized: "" or "some/prefix/" (trailing slash)
}

// Name identifies the destination in results and audit rows.
func (d *S3Destination) Name() string { return "s3" }

// Put uploads one local file to bucket/prefix+name.
func (d *S3Destination) Put(ctx context.Context, name, localPath string) error {
	if err := validName(name); err != nil {
		return err
	}
	return d.Store.Put(ctx, d.Bucket, d.Prefix+name, localPath)
}

// List returns object names under the prefix (prefix stripped).
func (d *S3Destination) List(ctx context.Context) ([]string, error) {
	objs, err := d.Store.List(ctx, d.Bucket, d.Prefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, strings.TrimPrefix(o.Key, d.Prefix))
	}
	return names, nil
}

// Delete removes one object under the prefix.
func (d *S3Destination) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return d.Store.Delete(ctx, d.Bucket, d.Prefix+name)
}

// BuildS3 constructs the production S3 destination from settings via the
// shared internal/s3 client. minio.New does not dial at construction time —
// endpoint/auth failures surface on the first upload, as a Result error.
func BuildS3(cfg model.BackupOffsiteS3) (*S3Destination, error) {
	store, err := s3.New(&model.S3Transfer{
		Endpoint:  cfg.Endpoint,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		UseTLS:    cfg.UseTLS,
	})
	if err != nil {
		return nil, err
	}
	return &S3Destination{Store: store, Bucket: cfg.Bucket, Prefix: cfg.Prefix}, nil
}
