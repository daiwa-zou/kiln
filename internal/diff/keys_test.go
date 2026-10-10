package diff

import (
	"slices"
	"testing"
)

func TestNamespaceSplitsDocumentsBySource(t *testing.T) {
	tests := []struct {
		key  Key
		want string
	}{
		{ModuleKey("auth"), PrefixModule},
		{DocKey("README.md"), PrefixDoc},
		{DocKey(UploadOrigin("a.pdf")), NamespaceUpload},
		{DocKey(UploadOrigin("a.pdf")) + "#intro", NamespaceUpload},
		{DocKey(WebOrigin("https://example.com/x")), NamespaceWeb},
		{ArchOverview, PrefixArch},
	}
	for _, tt := range tests {
		if got := Namespace(tt.key); got != tt.want {
			t.Errorf("Namespace(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestTalliedNamespacesOrder(t *testing.T) {
	// The overview is compared against the last one on every run, so the order
	// is part of its output.
	want := []string{"module", "doc", "doc:upload", "doc:web"}
	if !slices.Equal(TalliedNamespaces, want) {
		t.Errorf("TalliedNamespaces = %v, want %v", TalliedNamespaces, want)
	}
	for _, skipped := range []string{PrefixEntry, PrefixArch} {
		if slices.Contains(TalliedNamespaces, skipped) {
			t.Errorf("%q is deliberately not tallied as source material", skipped)
		}
	}
}
