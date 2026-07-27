package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadNewMasterKey(t *testing.T) {
	t.Setenv("KILN_NEW_MASTER_KEY", "")
	t.Setenv("KILN_NEW_MASTER_KEY_FILE", "")
	if _, err := readNewMasterKey(); err == nil || !strings.Contains(err.Error(), "KILN_NEW_MASTER_KEY") {
		t.Errorf("unset key error unhelpful: %v", err)
	}

	t.Setenv("KILN_NEW_MASTER_KEY", "env-key")
	if got, err := readNewMasterKey(); err != nil || got != "env-key" {
		t.Errorf("env form = %q, %v", got, err)
	}

	// The _FILE form wins over the environment and trims trailing newlines,
	// matching every other secret.
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_NEW_MASTER_KEY_FILE", path)
	if got, err := readNewMasterKey(); err != nil || got != "file-key" {
		t.Errorf("file form = %q, %v", got, err)
	}

	t.Setenv("KILN_NEW_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := readNewMasterKey(); err == nil {
		t.Error("missing key file accepted")
	}
}
