package git

import (
	"testing"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
)

func TestMapOfDegradesInsteadOfPanicking(t *testing.T) {
	if MapOf(nil) != nil {
		t.Error("MapOf(nil) should be nil")
	}

	// A set from another connector carries a different Native; the documented
	// contract is nil, not a panic, so git-aware callers degrade on document
	// workspaces.
	other := &connector.SourceSet{Kind: "upload", Native: "not a repo map"}
	if MapOf(other) != nil {
		t.Error("MapOf on a non-git set should be nil")
	}

	rm := &repomap.RepoMap{}
	carried := &connector.SourceSet{Kind: "git", Native: rm}
	if MapOf(carried) != rm {
		t.Error("MapOf did not return the carried repo map")
	}
}
