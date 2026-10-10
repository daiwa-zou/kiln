package api

import "testing"

func TestConnectorConfigValidatesBranch(t *testing.T) {
	base := func(branch any) map[string]any {
		return map[string]any{"url": "https://8.8.8.8/acme/repo.git", "branch": branch}
	}
	if msg := validateConnectorConfig("git", base("release/1.x")); msg != "" {
		t.Errorf("a plain branch was rejected: %s", msg)
	}
	if msg := validateConnectorConfig("git", base("")); msg != "" {
		t.Errorf("an empty branch (the default) was rejected: %s", msg)
	}
	for _, bad := range []any{"--upload-pack=x", "a b", 7} {
		if msg := validateConnectorConfig("git", base(bad)); msg == "" {
			t.Errorf("branch %v was accepted", bad)
		}
	}
}
