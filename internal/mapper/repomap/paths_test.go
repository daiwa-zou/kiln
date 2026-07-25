package repomap

import "testing"

func TestRelativeTo(t *testing.T) {
	tests := []struct {
		dir, file, want string
	}{
		{".", "cmd/auth/main.go", "cmd/auth/main.go"},
		{"", "README.md", "README.md"},
		{"apps/ripple", "apps/ripple/main.go", "main.go"},
		{"apps/ripple", "apps/ripple/internal/task/task.go", "internal/task/task.go"},
		{"apps/ripple", "apps/beacon/main.go", ""},
		// A prefix match on the string must not be mistaken for a path match.
		{"apps/rip", "apps/ripple/main.go", ""},
		{"apps/ripple", "apps/ripple", "."},
		{"./apps/ripple", "apps/ripple/main.go", "main.go"},
	}

	for _, tt := range tests {
		t.Run(tt.dir+"|"+tt.file, func(t *testing.T) {
			if got := relativeTo(tt.dir, tt.file); got != tt.want {
				t.Errorf("relativeTo(%q, %q) = %q, want %q", tt.dir, tt.file, got, tt.want)
			}
		})
	}
}

func TestOwningDir(t *testing.T) {
	// The flowbit shape: a root module plus nested app modules. Longest prefix
	// must win, or every change is attributed to the root.
	dirs := []string{".", "apps/ripple", "apps/beacon"}

	tests := []struct {
		file, want string
	}{
		{"apps/ripple/main.go", "apps/ripple"},
		{"apps/ripple/internal/task/task.go", "apps/ripple"},
		{"apps/beacon/main.go", "apps/beacon"},
		{"Makefile", "."},
		{"apps/shared-notes.md", "."},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			if got := owningDir(dirs, tt.file); got != tt.want {
				t.Errorf("owningDir(%q) = %q, want %q", tt.file, got, tt.want)
			}
		})
	}
}

func TestOwningDirNoRootModule(t *testing.T) {
	// The InfraFlux shape: sibling modules with no root manifest, so a file
	// outside every module belongs to none of them.
	dirs := []string{"shared", "infraflux", "pilot", "node"}

	if got := owningDir(dirs, "pilot/cmd/main.go"); got != "pilot" {
		t.Errorf("owningDir = %q, want pilot", got)
	}
	if got := owningDir(dirs, "protobufs/README.md"); got != "" {
		t.Errorf("owningDir = %q, want empty (no module owns it)", got)
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"apps/ripple", "apps-ripple"},
		{"InfraFlux", "infraflux"},
		{"github.com/daiwa-zou/ripple", "github-com-daiwa-zou-ripple"},
		{"src/db", "src-db"},
		{"  spaced  out  ", "spaced-out"},
		{"already-slugged", "already-slugged"},
		{"---leading-and-trailing---", "leading-and-trailing"},
		{".", "root"},
		{"", "root"},
		{"!!!", "root"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := Slugify(tt.in); got != tt.want {
				t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSlugifyIsIdempotent(t *testing.T) {
	inputs := []string{"apps/ripple", "InfraFlux", "github.com/x/y", "  a  b  ", "!!!"}
	for _, in := range inputs {
		once := Slugify(in)
		if twice := Slugify(once); twice != once {
			t.Errorf("Slugify not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestUniqueSlug(t *testing.T) {
	taken := map[string]bool{}

	if got := uniqueSlug(taken, "shared"); got != "shared" {
		t.Errorf("first = %q, want shared", got)
	}
	if got := uniqueSlug(taken, "shared"); got != "shared-2" {
		t.Errorf("second = %q, want shared-2", got)
	}
	if got := uniqueSlug(taken, "shared"); got != "shared-3" {
		t.Errorf("third = %q, want shared-3", got)
	}
	if got := uniqueSlug(taken, "other"); got != "other" {
		t.Errorf("unrelated = %q, want other", got)
	}
}
