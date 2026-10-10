// The fs driver exists so a single-node deployment needs no object store at
// all: blobs live under one directory on a mounted volume. Every access goes
// through os.Root, so even a key that somehow survived validKey cannot escape
// the root. Writes are temp-then-rename for atomicity -- a crashed upload
// leaves a *.tmp straggler, never a torn blob.
package blob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
)

type fsStore struct {
	root *os.Root
}

func openFS(dir string) (*fsStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("blob: fs backend needs storage.path")
	}
	// Uploaded documents live here for good, unlike the per-run temp dirs, so
	// the tree is closed to other local users. Group read/traverse stays for a
	// volume shared through fsGroup.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blob: create storage root: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: open storage root: %w", err)
	}
	return &fsStore{root: root}, nil
}

func (s *fsStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := validKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("blob: put %s: %w", key, err)
		}
	}
	// A random suffix rather than os.CreateTemp: os.Root has no CreateTemp,
	// and two concurrent puts of the same key must not share a temp file.
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	tmp := key + "." + hex.EncodeToString(buf[:]) + ".tmp"
	f, err := s.root.Create(tmp)
	if err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	// Sync before rename is what makes "never a torn blob" true across power
	// loss: rename only orders the directory entry, and without an fsync of
	// the data a filesystem may persist the new name first and leave a
	// zero-length file behind it. Close can also be where a deferred write
	// error surfaces, so its result counts too -- but the first failure is
	// the one worth reporting.
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = s.root.Remove(tmp) // best effort; a stray .tmp is inert
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	if err := s.root.Rename(tmp, key); err != nil {
		_ = s.root.Remove(tmp)
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	s.syncDir(path.Dir(key))
	return nil
}

// syncDir fsyncs a directory so a completed rename survives power loss. It is
// best effort: the blob is already in place and durable-by-data, and some
// filesystems refuse to fsync a directory, which must not turn a stored blob
// into a failed upload.
func (s *fsStore) syncDir(dir string) {
	d, err := s.root.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func (s *fsStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.root.Open(key)
	if err != nil {
		return nil, fmt.Errorf("blob: get %s: %w", key, err)
	}
	return f, nil
}

func (s *fsStore) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}
