package api

import (
	"strings"
	"testing"
)

// A viewer's write answers 403 with errRoleReadOnly, and the legacy UI turns
// exactly that message into read-only copy. The match is on the literal, so
// the test reads the Go const rather than retyping it: rewording the server
// message without updating app.js fails here, not in front of a viewer.
func TestUIMapsTheRoleRefusal(t *testing.T) {
	src := uiSource(t, "app.js")

	for _, want := range []string{
		`res.status === 403 && msg === "` + errRoleReadOnly + `"`,
		"You have read-only access to this bench. Ask an owner to change your role.",
		"err.forbidden = true",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js does not map the role refusal: missing %q", want)
		}
	}
}
