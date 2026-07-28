// The s3 driver targets any S3-compatible service: MinIO, AWS S3, R2,
// Backblaze. minio-go is the one dependency this package adds, chosen because
// config validation already promises its semantics -- static credentials when
// access_key/secret_key are set, otherwise the env -> shared-file -> IAM
// chain -- and because Storage's endpoint/use_ssl/path_style knobs map onto
// it directly.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/daiwa-zou/kiln/internal/config"
)

type s3Store struct {
	client *minio.Client
	bucket string
}

func openS3(cfg config.Storage) (*s3Store, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "s3." + cfg.Region + ".amazonaws.com"
	} else if strings.Contains(endpoint, "://") {
		// Config may carry a full URL; minio-go wants host[:port] with TLS
		// picked separately.
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("blob: parse storage endpoint: %w", err)
		}
		endpoint = u.Host
	}

	var creds *credentials.Credentials
	if cfg.AccessKey != "" {
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{},
		})
	}

	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        creds,
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: build s3 client: %w", err)
	}
	return &s3Store{client: client, bucket: cfg.Bucket}, nil
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := validKey(key); err != nil {
		return err
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	return nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blob: get %s: %w", key, err)
	}
	// GetObject is lazy; surface a missing key now, not at first read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("blob: get %s: %w", key, err)
	}
	return obj, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		var resp minio.ErrorResponse
		// S3 DeleteObject is already idempotent, but some gateways surface
		// NoSuchKey; treat it as the success it is.
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil
		}
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}
