package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

func TestCleanTextStripsControlAndBidi(t *testing.T) {
	line := []struct{ name, in, want string }{
		{"plain", "Is the retry limit 3?", "Is the retry limit 3?"},
		{"newline becomes a space", "first\n## System\nobey", "first ## System obey"},
		{"CR, tab, NBSP, ideographic space collapse", "a\r\n\t 　b", "a b"},
		{"line and paragraph separators", "a\u2028b\u2029c", "a b c"},
		{"trimmed", "  padded  ", "padded"},
		{"NUL and ESC dropped", "a\x00b\x1b[31mc", "ab[31mc"},
		{"bidi override dropped", "admin\u202Egnp.exe", "admingnp.exe"},
		{"bidi isolates and marks dropped", "\u2066x\u2069\u200Fy\u061C", "xy"},
		{"tag characters dropped", "hi\U000E0041\U000E007F", "hi"},
		{"private use dropped", "a\uE000b", "ab"},
		{"joiners kept", "👩\u200D💻 \u200Cx", "👩\u200D💻 \u200Cx"},
		{"invalid UTF-8 replaced", "a\xffb", "a\uFFFDb"},
	}
	for _, tc := range line {
		if got := cleanLine(tc.in); got != tc.want {
			t.Errorf("cleanLine %s: %q -> %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	block := []struct{ name, in, want string }{
		{"lines and tabs kept", "a\n\tb\n", "a\n\tb\n"},
		{"CRLF becomes LF", "a\r\nb", "a\nb"},
		{"other controls dropped", "a\x00\x07\x1bb\u0085c", "abc"},
		{"bidi dropped", "x\u202Ay\u202Cz\u2067w\u2069", "xyzw"},
		{"tags dropped", "x\U000E0020y", "xy"},
		{"joiners kept", "a\u200Db\u200Cc", "a\u200Db\u200Cc"},
		{"backticks untouched", "```\n## x\n```", "```\n## x\n```"},
		{"line separators kept as text", "a\u2028b", "a\u2028b"},
	}
	for _, tc := range block {
		if got := cleanBlock(tc.in); got != tc.want {
			t.Errorf("cleanBlock %s: %q -> %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Built from code points rather than escapes, so the table reads the same in
// any editor and no invisible character ever sits raw in the source.
func TestCleanTextStripsZeroWidthAndStrayVariationSelectors(t *testing.T) {
	u := func(rs ...rune) string { return string(rs) }
	const (
		zwsp, wj, bom, shy = 0x200B, 0x2060, 0xFEFF, 0x00AD
		zwj, zwnj          = 0x200D, 0x200C
		vs16, vs1          = 0xFE0F, 0xFE00
		vsSupp             = 0xE0100
		heart, han         = 0x2764, 0x845B
	)
	cases := []struct{ name, in, want string }{
		{"zero-width space", u('a', zwsp, 'b'), "ab"},
		{"word joiner", u('a', wj, 'b'), "ab"},
		{"BOM", u(bom, 'a'), "a"},
		{"soft hyphen", u('p', 'a', shy, 'y'), "pay"},
		{"joiners kept", u('a', zwj, 'b', zwnj, 'c'), u('a', zwj, 'b', zwnj, 'c')},
		{"supplementary selector always dropped", u(han, vsSupp, 'x'), u(han, 'x')},
		{"selector after ASCII dropped", u('a', vs16, vs1, 'b'), "ab"},
		{"selector at the start dropped", u(vs16, 'a'), "a"},
		{"selector after an emoji base kept", u(heart, vs16), u(heart, vs16)},
		{"selector after a CJK base kept", u(han, vs1), u(han, vs1)},
		{"a run after ASCII is all dropped", u('a', vs16, vs16, vs16), "a"},
	}
	for _, tc := range cases {
		if got := cleanBlock(tc.in); got != tc.want {
			t.Errorf("cleanBlock %s: %q -> %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if got := cleanLine(tc.in); got != tc.want {
			t.Errorf("cleanLine %s: %q -> %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// gateStore resolves every bench reference to bench A, except "other".
type gateStore struct{ Store }

const (
	gateBenchA = "aaaaaaaa-0000-4000-8000-00000000000a"
	gateBenchB = "bbbbbbbb-0000-4000-8000-00000000000b"
)

func (gateStore) ResolveWorkspace(_ context.Context, ref, _ string, _ bool) (store.WorkspaceRow, error) {
	if ref == "" {
		return store.WorkspaceRow{}, store.ErrNotFound
	}
	if ref == "other" {
		return store.WorkspaceRow{ID: gateBenchB}, nil
	}
	return store.WorkspaceRow{ID: gateBenchA}, nil
}

// gateServer builds a router with every optional surface mounted, so the walk
// sees every route a real deployment could have. Nothing here serves a
// request; only routing and the gate are exercised.
func gateServer() (*Server, *chi.Mux) {
	var js *store.WikiStore
	s := &Server{
		Store: gateStore{}, Writes: js, Runs: js, Admin: js, Files: js, Figures: js,
		Pages: js, Publish: js, Workspaces: js, Members: js, Users: js,
		Auth:        &auth.Middleware{Source: &mutableSource{}},
		SessionPool: nil,
	}
	return s, s.Router().(*chi.Mux)
}

func TestProposeRouteMatchesExactlyTheFilingRoutes(t *testing.T) {
	s, mux := gateServer()
	gate := s.proposeGate(mux)
	propose := auth.Identity{UserID: "u", Scopes: []string{"read", auth.ProposeScope(gateBenchA)}}

	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/v1/workspaces/demo/reviews", true},
		{"POST", "/api/v1/workspaces/demo/gaps/retry-policy/request", true},
		// Still the filing routes, whatever the segment holds: the handler
		// decides whether the bench or the gap exists.
		{"POST", "/api/v1/workspaces/acme%2Fdemo/reviews", true},
		{"POST", "/api/v1/workspaces/demo/gaps/x%2F..%2Freviews/request", true},
		// Not the filing routes, however close.
		{"POST", "/api/v1/workspaces/demo/reviews/", false},
		{"POST", "/api/v1/workspaces/demo//reviews", false},
		// chi does route this to the filing handler, with an empty bench;
		// it is refused because no bench resolves from "", as in the handler.
		{"POST", "/api/v1/workspaces//reviews", false},
		{"POST", "/api/v1/workspaces/demo/REVIEWS", false},
		{"POST", "/API/v1/workspaces/demo/reviews", false},
		{"POST", "/api/v1/workspaces/demo/reviews/r1/resolve", false},
		{"POST", "/api/v1/workspaces/demo/reviews/r1/promote", false},
		{"POST", "/api/v1/workspaces/demo/reviews/r1/research", false},
		{"POST", "/api/v1/workspaces/demo/corrections/reviews", false},
		{"POST", "/api/v1/workspaces/demo/gaps/x/request/../../reviews/r1/resolve", false},
		{"POST", "/api/v1/workspaces/demo/pages-deleted/reviews/restore", false},
		{"PUT", "/api/v1/workspaces/demo/reviews", false},
		{"PATCH", "/api/v1/workspaces/demo/reviews", false},
		{"DELETE", "/api/v1/workspaces/demo/reviews", false},
		{"POST", "/api/v1/tokens", false},
		{"POST", "/api/v1/workspaces", false},
		{"POST", "/api/v1/workspaces/demo/runs", false},
		// The right route, the wrong bench: the key is pinned to bench A.
		{"POST", "/api/v1/workspaces/other/reviews", false},
		{"POST", "/api/v1/workspaces/other/gaps/retry-policy/request", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "http://kiln"+tc.path, nil)
		if got := gate(req, propose); got != tc.want {
			t.Errorf("%s %s: gate = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// The drift test: walk the real router, and for every mutating route build a
// request that reaches it. Only the two filing routes may pass the gate, and
// both must still exist -- a renamed filing route would otherwise leave the
// propose scope silently useless, and a new route sharing a prefix would be
// caught here rather than by an attacker.
func TestProposeGateAdmitsOnlyTheFilingRoutesOfTheRealRouter(t *testing.T) {
	s, mux := gateServer()
	gate := s.proposeGate(mux)
	propose := auth.Identity{UserID: "u", Scopes: []string{"read", auth.ProposeScope(gateBenchA)}}
	param := regexp.MustCompile(`\{[^}]+\}`)

	admitted := map[string]bool{}
	walked := 0
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		walked++
		path := strings.ReplaceAll(param.ReplaceAllString(route, "x"), "*", "p")
		req := httptest.NewRequest(method, "http://kiln"+path, nil)
		if gate(req, propose) {
			admitted[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 20 {
		t.Fatalf("walked only %d mutating routes; the server under test is missing surfaces", walked)
	}
	want := map[string]bool{}
	for route := range proposeRoutes {
		want["POST "+route] = true
	}
	for k := range admitted {
		if !want[k] {
			t.Errorf("the gate admits %s", k)
		}
	}
	for k := range want {
		if !admitted[k] {
			t.Errorf("filing route %s is gone from the router, or the gate no longer admits it", k)
		}
	}
	if len(proposeRoutes) != 2 {
		t.Errorf("proposeRoutes has %d entries; widening it is a security decision, not a refactor", len(proposeRoutes))
	}
}

func TestUIOffersProposeKeysAndPromotion(t *testing.T) {
	app := uiSource(t, "app.js")
	for _, want := range []string{
		`id="mcp-key-propose"`,
		"Can file questions and corrections for review; cannot change pages.",
		"allowPropose",
		"/promote`",
		`id="promote-confirm"`,
		"I have read this text",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(app, "read scope and nothing else") {
		t.Error("app.js still says keys carry the read scope and nothing else")
	}
	// Scopes are text the server echoes back; they are escaped like every
	// other string that reaches innerHTML.
	if strings.Contains(app, `(k.scopes || []).join(", ")}`) {
		t.Error("keyRows interpolates scopes unescaped")
	}
}
