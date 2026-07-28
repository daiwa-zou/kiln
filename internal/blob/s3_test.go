package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/config"
)

// fakeS3 is the smallest S3 the minio client will talk to: path-style object
// PUT/GET/HEAD/DELETE on one bucket, in memory. It keeps the driver's whole
// request path hermetically testable; real-endpoint behavior stays covered by
// the gated round-trip test below.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Over plain HTTP the client streams with the SigV4 chunked encoding;
		// strip the "size;chunk-signature=…\r\n" framing down to the payload.
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			body = decodeAWSChunked(body)
		}
		f.objects[key] = body
		w.Header().Set("ETag", `"fake"`)
	case http.MethodHead, http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `<Error><Code>NoSuchKey</Code><Key>%s</Key></Error>`, key)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("ETag", `"fake"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// decodeAWSChunked unframes the SigV4 streaming payload: each chunk is
// "hexsize;chunk-signature=…\r\n<data>\r\n", terminated by a zero-size chunk.
func decodeAWSChunked(in []byte) []byte {
	var out []byte
	for {
		nl := bytes.Index(in, []byte("\r\n"))
		if nl < 0 {
			return out
		}
		header := string(in[:nl])
		in = in[nl+2:]
		size := 0
		if _, err := fmt.Sscanf(strings.SplitN(header, ";", 2)[0], "%x", &size); err != nil || size == 0 {
			return out
		}
		if size > len(in) {
			return out
		}
		out = append(out, in[:size]...)
		in = in[size:]
		in = bytes.TrimPrefix(in, []byte("\r\n"))
	}
}

func TestS3DriverAgainstFakeServer(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s, err := Open(config.Storage{
		Backend:   config.BackendS3,
		Endpoint:  srv.URL, // full URL form: exercises the scheme-stripping path
		Bucket:    "kiln-test",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "us-east-1",
		UseSSL:    false,
		PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := FileKey("ws-fake", "f-fake")
	content := []byte("fired clay, fake shelf")

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

	if _, err := s.Get(ctx, "ws-fake/absent"); err == nil {
		t.Fatal("get of an absent key succeeded")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Fatal("get after delete succeeded")
	}
	// Idempotent: deleting an absent key is success.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	// Key validation short-circuits before any request.
	if err := s.Put(ctx, "../escape", bytes.NewReader(nil), 0); err == nil {
		t.Fatal("traversal key accepted")
	}
	if _, err := s.Get(ctx, ""); err == nil {
		t.Fatal("empty key accepted")
	}
	if err := s.Delete(ctx, "/abs"); err == nil {
		t.Fatal("absolute key accepted")
	}
}

func TestOpenS3ConfigShapes(t *testing.T) {
	// Bare host endpoint and the credential chain (no static keys).
	if _, err := openS3(config.Storage{
		Backend: config.BackendS3, Endpoint: "minio.internal:9000",
		Bucket: "kiln", Region: "us-east-1", PathStyle: true,
	}); err != nil {
		t.Fatalf("bare-host endpoint: %v", err)
	}
	// No endpoint falls back to the regional AWS host, DNS bucket lookup.
	if _, err := openS3(config.Storage{
		Backend: config.BackendS3, Bucket: "kiln", Region: "eu-west-1",
		AccessKey: "a", SecretKey: "b", UseSSL: true,
	}); err != nil {
		t.Fatalf("aws default endpoint: %v", err)
	}
	// A malformed endpoint URL is a configuration error, not a panic later.
	if _, err := openS3(config.Storage{
		Backend: config.BackendS3, Endpoint: "http://bad host/%", Bucket: "kiln",
	}); err == nil {
		t.Fatal("malformed endpoint accepted")
	}
}

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
