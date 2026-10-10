package jobs

import (
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
)

// stubDocConnector lets a test drive syncDocSource with documents it builds
// itself. The real web connector refuses loopback, so a materialize test could
// not fetch a page from httptest; the payload closure on docSource is the
// seam that makes the connector's own result irrelevant.
type stubDocConnector struct{}

const stubDocKind = "test-jobs-docs"

func (stubDocConnector) Kind() string { return stubDocKind }
func (stubDocConnector) Sync(context.Context, connector.Config, string) (*connector.SourceSet, error) {
	return &connector.SourceSet{Kind: stubDocKind}, nil
}

func init() { connector.Register(stubDocConnector{}) }

// twoChapters is past docmap.SplitMinBytes, with two top-level headings, so
// docmap splits it into a parent unit and two section units.
func twoChapters(title string) string {
	body := strings.Repeat("Some prose that fills the chapter out. ", 400)
	return "# " + title + " one\n\n" + body + "\n\n# " + title + " two\n\n" + body + "\n"
}

func syncStubDocs(t *testing.T, origin func(string) string, renames bool, docs []docmap.Doc) docSync {
	t.Helper()
	res, err := syncDocSource(context.Background(), io.Discard, docSource{
		kind:    stubDocKind,
		subject: "test",
		prefix:  "kiln-absorb-",
		noun:    "doc(s)",
		config:  connector.Config{},
		renames: renames,
		origin:  origin,
		payload: func(*connector.SourceSet) ([]docmap.Doc, [][2]string, bool) {
			return docs, nil, true
		},
	})
	if err != nil {
		t.Fatalf("syncDocSource: %v", err)
	}
	t.Cleanup(func() { m := &material{staging: []string{res.Staging}}; m.Close() })
	return res
}

// TestAbsorbKeepsEveryDocumentSource pins what the documents and web blocks of
// materialize used to get right only by running in a fixed order: sections,
// routes and figures from one source must survive the other being absorbed,
// whichever goes first.
func TestAbsorbKeepsEveryDocumentSource(t *testing.T) {
	uploadKey := diff.DocKey(diff.UploadOrigin("guide.md"))
	webKey := diff.DocKey(diff.WebOrigin("https://example.com/page"))
	uploadFig := extract.Figure{Ref: "p1-i1", Caption: "upload figure", SHA256: "aa"}
	webFig := extract.Figure{Ref: "remote", Caption: "web figure", SHA256: "bb"}

	build := func(t *testing.T) (upload, web docSync) {
		upload = syncStubDocs(t, diff.UploadOrigin, true, []docmap.Doc{{
			Key: string(uploadKey), Path: "guide.md", Title: "Guide", Origin: "guide.md",
			Text: twoChapters("Guide"), Hash: "h1", Figures: []extract.Figure{uploadFig},
		}})
		web = syncStubDocs(t, diff.WebOrigin, false, []docmap.Doc{{
			Key: string(webKey), Path: "page.md", Title: "Page", Origin: "https://example.com/page",
			Text: twoChapters("Page"), Hash: "h2", Figures: []extract.Figure{webFig},
		}})
		return upload, web
	}

	for name, order := range map[string]string{"documents then web": "uw", "web then documents": "wu"} {
		t.Run(name, func(t *testing.T) {
			upload, web := build(t)
			m := &material{
				Map:    &mapper.WorkspaceMap{SchemaVersion: 1},
				Router: diff.Router{DocPaths: map[string]diff.Key{}},
			}
			for _, which := range order {
				s := upload
				if which == 'w' {
					s = web
				}
				if err := m.absorb("", s); err != nil {
					t.Fatalf("absorb: %v", err)
				}
			}

			// Section units were generated, or this test proves nothing.
			if len(upload.Map.Units) < 3 || len(web.Map.Units) < 3 {
				t.Fatalf("documents were not split into sections: %d and %d units",
					len(upload.Map.Units), len(web.Map.Units))
			}

			for _, parent := range []diff.Key{uploadKey, webKey} {
				if got := m.Router.DocSections[parent]; len(got) != 2 {
					t.Errorf("sections routed under %s = %v, want its two chapters", parent, got)
				}
			}
			if got := m.Router.DocPaths[diff.UploadOrigin("guide.md")]; got != uploadKey {
				t.Errorf("upload route = %q, want %q", got, uploadKey)
			}
			if got := m.Router.DocPaths[diff.WebOrigin("https://example.com/page")]; got != webKey {
				t.Errorf("web route = %q, want %q", got, webKey)
			}

			if got := m.Figures[string(uploadKey)]; len(got) != 1 || got[0].Ref != uploadFig.Ref {
				t.Errorf("upload figures = %v", got)
			}
			if got := m.Figures[string(webKey)]; len(got) != 1 || got[0].Ref != webFig.Ref {
				t.Errorf("web figures = %v", got)
			}

			// Names are only ever written back to stored upload rows. A fetched
			// page has none, so absorbing one must not add an entry for it.
			if want := map[string]string{"guide.md": "Guide"}; !maps.Equal(m.DocNames, want) {
				t.Errorf("DocNames = %v, want %v", m.DocNames, want)
			}

			keys := make([]string, 0, len(m.Map.Units))
			for _, u := range m.Map.Units {
				keys = append(keys, u.Key)
			}
			for _, k := range []diff.Key{uploadKey, webKey} {
				if !slices.Contains(keys, string(k)) {
					t.Errorf("merged map lost %s; has %v", k, keys)
				}
			}
		})
	}
}
