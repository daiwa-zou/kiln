package jobs

import (
	"os"
	"path/filepath"
)

// writeFile writes content at dir/rel, creating parents. Used by the scripted
// runner to simulate agent output.
func writeFile(dir, rel, content string) error {
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}
