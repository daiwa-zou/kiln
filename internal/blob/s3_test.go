package blob

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/daiwa-zou/kiln/internal/config"
)

// TestS3RoundTrip exercises the s3 driver against a real endpoint (a local
// MinIO in practice). Gated on KILN_TEST_S3_ENDPOINT so the default suite
// stays offline, matching how database tests key off KILN_TEST_DATABASE_URL.
//
//	docker run --rm -p 9000:9000 minio/minio server /data
//	KILN_TEST_S3_ENDPOINT=localhost:9000 KILN_TEST_S3_BUCKET=kiln-test \
//	  KILN_TEST_S3_ACCESS_KEY=minioadmin KILN_TEST_S3_SECRET_KEY=minioadmin \
//	  go test ./internal/blob/
func TestS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("KILN_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("KILN_TEST_S3_ENDPOINT not set; skipping S3 integration test")
	}
	s, err := openS3(config.Storage{
		Backend:   config.BackendS3,
		Endpoint:  endpoint,
		Bucket:    os.Getenv("KILN_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("KILN_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("KILN_TEST_S3_SECRET_KEY"),
		Region:    "us-east-1",
		PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := FileKey("ws-test", "f-test")
	content := []byte("fired clay, remote shelf")

	if err := s.Put(ctx, key, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("got %q err %v", got, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Fatal("get after delete succeeded")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
