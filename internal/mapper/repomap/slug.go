package repomap

import "github.com/daiwa-zou/kiln/internal/mapper"

// Slugify converts a directory path or name into a slug, with "root" standing
// in when nothing survives (the repository root is "." and has no name of its
// own). The algorithm lives in mapper.Slugify so that every mapper derives page
// identity the same way; only the fallback word is repomap's.
func Slugify(s string) string {
	return mapper.Slugify(s, "root")
}
