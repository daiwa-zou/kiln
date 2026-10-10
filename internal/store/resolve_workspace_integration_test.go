package store

import (
	"context"
	"errors"
	"testing"
)

// resolveFixture builds the attack's setup: two orgs that each own a "docs"
// bench, a user who belongs to both (the attacker's org added them), a user who
// belongs only to the victim's org, and an unrelated "solo" bench.
type resolveFixture struct {
	s                  *WikiStore
	evilDocs, goodDocs string // workspace ids; evilDocs is the older one
	both, goodOnly     string // user ids
	solo               string
}

func newResolveFixture(t *testing.T) resolveFixture {
	t.Helper()
	pool := testPool(t)
	freshSchema(t, pool)
	ctx := context.Background()
	s := NewWikiStore(pool)

	// The attacker's bench is created first, so the old resolver (oldest
	// wins) would have chosen it for everyone.
	evil, err := s.EnsureWorkspace(ctx, "evil-co", "docs", "Docs")
	if err != nil {
		t.Fatal(err)
	}
	good, err := s.EnsureWorkspace(ctx, "good-co", "docs", "Docs")
	if err != nil {
		t.Fatal(err)
	}
	solo, err := s.EnsureWorkspace(ctx, "good-co", "solo", "Solo")
	if err != nil {
		t.Fatal(err)
	}

	user := func(login, wsID string, others ...string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (login) VALUES ($1) RETURNING id`, login).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for _, w := range append([]string{wsID}, others...) {
			if _, err := pool.Exec(ctx, `
				INSERT INTO org_members (org_id, user_id, role)
				SELECT org_id, $2, 'member' FROM workspaces WHERE id = $1`, w, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	return resolveFixture{
		s: s, evilDocs: evil, goodDocs: good, solo: solo,
		both:     user("victim", good, evil),
		goodOnly: user("gary", good),
	}
}

// The core of the fix: a user in two orgs that both have "docs" must not get
// one of them picked for them.
func TestResolveWorkspaceBareSlugAmbiguousAcrossOrgs(t *testing.T) {
	f := newResolveFixture(t)
	ctx := context.Background()

	_, err := f.s.ResolveWorkspace(ctx, "docs", f.both, false)
	if !errors.Is(err, ErrAmbiguousWorkspace) {
		t.Fatalf("bare slug in two visible orgs: want ErrAmbiguousWorkspace, got %v", err)
	}
	var amb *AmbiguousWorkspaceError
	if !errors.As(err, &amb) {
		t.Fatalf("want *AmbiguousWorkspaceError, got %T", err)
	}
	got := map[string]string{}
	for _, c := range amb.Candidates {
		got[c.Ref()] = c.ID
	}
	if len(got) != 2 || got["evil-co/docs"] != f.evilDocs || got["good-co/docs"] != f.goodDocs {
		t.Fatalf("candidates = %v, want both docs benches with their ids", got)
	}

	// Instance admins get no special pick either: they see every org.
	if _, err := f.s.ResolveWorkspace(ctx, "docs", "", true); !errors.Is(err, ErrAmbiguousWorkspace) {
		t.Fatalf("admin bare slug: want ErrAmbiguousWorkspace, got %v", err)
	}
}

func TestResolveWorkspaceByIDAndOrgSlug(t *testing.T) {
	f := newResolveFixture(t)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		ref, wantID, wantOrg string
	}{
		"id of the victim's own bench":   {f.goodDocs, f.goodDocs, "good-co"},
		"id of the attacker's bench":     {f.evilDocs, f.evilDocs, "evil-co"},
		"org/slug, victim's":             {"good-co/docs", f.goodDocs, "good-co"},
		"org/slug, attacker's":           {"evil-co/docs", f.evilDocs, "evil-co"},
		"uppercase id is still the id":   {upper(f.goodDocs), f.goodDocs, "good-co"},
		"bare slug that is not shared":   {"solo", f.solo, "good-co"},
		"id of a bench with unique slug": {f.solo, f.solo, "good-co"},
	} {
		t.Run(name, func(t *testing.T) {
			ws, err := f.s.ResolveWorkspace(ctx, tc.ref, f.both, false)
			if err != nil {
				t.Fatalf("ResolveWorkspace(%q): %v", tc.ref, err)
			}
			if ws.ID != tc.wantID || ws.Org != tc.wantOrg || ws.Slug == "" {
				t.Fatalf("ResolveWorkspace(%q) = %+v, want id %s org %s", tc.ref, ws, tc.wantID, tc.wantOrg)
			}
		})
	}
}

// A user in only one org has nothing to disambiguate, so the bare slug keeps
// working and cannot reach the other org's bench.
func TestResolveWorkspaceBareSlugUniqueWhenOnlyOneVisible(t *testing.T) {
	f := newResolveFixture(t)
	ctx := context.Background()

	ws, err := f.s.ResolveWorkspace(ctx, "docs", f.goodOnly, false)
	if err != nil {
		t.Fatalf("single-org user, bare slug: %v", err)
	}
	if ws.ID != f.goodDocs {
		t.Fatalf("resolved %s, want their own org's bench %s (not the older one in an org they are not in)", ws.ID, f.goodDocs)
	}
}

// Visibility is a bound on every reference form. An invisible workspace is not
// found, rather than forbidden, so existence cannot be probed.
func TestResolveWorkspaceVisibilityIsEnforced(t *testing.T) {
	f := newResolveFixture(t)
	ctx := context.Background()

	for name, ref := range map[string]string{
		"id":       f.evilDocs,
		"org/slug": "evil-co/docs",
	} {
		if _, err := f.s.ResolveWorkspace(ctx, ref, f.goodOnly, false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s of an org the caller is not in: want ErrNotFound, got %v", name, err)
		}
	}
	// No identity and not admin sees nothing at all.
	if _, err := f.s.ResolveWorkspace(ctx, f.goodDocs, "", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("anonymous non-admin: want ErrNotFound, got %v", err)
	}
	// And ambiguity is judged over what the caller can see, never over what
	// exists: the one-org user must not learn a second "docs" exists.
	if _, err := f.s.ResolveWorkspace(ctx, "docs", f.goodOnly, false); errors.Is(err, ErrAmbiguousWorkspace) {
		t.Errorf("ambiguity leaked a bench the caller cannot see: %v", err)
	}
}

func TestResolveWorkspaceRejectsMalformedReferences(t *testing.T) {
	f := newResolveFixture(t)
	ctx := context.Background()

	for _, ref := range []string{
		"", "/", "good-co/", "/docs", "good-co/docs/extra", "no-such-org/docs", "good-co/no-such",
		"00000000-0000-0000-0000-000000000000", "not-a-uuid-but-has-dashes",
	} {
		if _, err := f.s.ResolveWorkspace(ctx, ref, f.both, true); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveWorkspace(%q): want ErrNotFound, got %v", ref, err)
		}
	}
}

func TestListWorkspacesCarriesOrg(t *testing.T) {
	f := newResolveFixture(t)
	rows, err := f.s.ListWorkspaces(context.Background(), f.both, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 visible benches, got %d: %+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Org == "" {
			t.Errorf("row %+v has no org; the UI needs it to tell same-slug benches apart", r)
		}
		seen[r.Org+"/"+r.Slug] = true
	}
	if !seen["evil-co/docs"] || !seen["good-co/docs"] || !seen["good-co/solo"] {
		t.Errorf("unexpected set: %v", seen)
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}
