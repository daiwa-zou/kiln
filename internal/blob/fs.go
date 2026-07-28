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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
		if err := s.root.MkdirAll(dir, 0o755); err != nil {
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
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		_ = s.root.Remove(tmp) // best effort; a stray .tmp is inert
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		_ = s.root.Remove(tmp)
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	if err := s.root.Rename(tmp, key); err != nil {
		_ = s.root.Remove(tmp)
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	return nil
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
