// Package blob is the object storage client the rest of kiln was built to
// expect: config, validation, secrets, sources.blob_keys, and the cascade's
// DeleteBlobs plan all predate it. The interface is the smallest surface the
// callers need -- streaming put, get, and idempotent delete; there is no list,
// because everything stored is tracked in Postgres and an untracked blob is
// garbage by definition.
//
// Keys are never user-controlled. Callers build them from server-generated
// UUIDs via FileKey, so tenant isolation holds by construction; validKey is
// defense in depth, not the boundary.
package blob

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/daiwa-zou/kiln/internal/config"
)

// Store reads and writes blobs by key.
type Store interface {
	// Put streams a blob under key, replacing any existing content. size is
	// the byte count when the caller knows it, or -1 to let the driver find
	// out; either way the content is streamed, not buffered whole.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens a blob for reading. The caller closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes a blob. Deleting an absent key is not an error: deletes
	// run best-effort after commits, and a retry must not fail on success.
	Delete(ctx context.Context, key string) error
}

// Open selects a driver from resolved storage config.
func Open(cfg config.Storage) (Store, error) {
	switch cfg.Backend {
	case config.BackendFS:
		return openFS(cfg.Path)
	case config.BackendS3:
		return openS3(cfg)
	default:
		return nil, fmt.Errorf("blob: unknown storage backend %q (want s3 or fs)", cfg.Backend)
	}
}

// FileKey builds the canonical key for an uploaded workspace file. Both ids
// are server-generated UUIDs; no user input ever reaches a key.
func FileKey(workspaceID, fileID string) string {
	return "ws/" + workspaceID + "/uploads/" + fileID
}

// validKey rejects keys that could escape a prefix or a filesystem root:
// empty keys, absolute paths, dot and dot-dot segments, and empty segments.
func validKey(key string) error {
	if key == "" {
		return fmt.Errorf("blob: empty key")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("blob: absolute key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return fmt.Errorf("blob: empty segment in key %q", key)
		case ".", "..":
			return fmt.Errorf("blob: dot segment in key %q", key)
		}
	}
	return nil
}
