package wiki

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A figure reference is a citation, not an upload.
//
// `![caption](figure:ID)` names a picture the pipeline already stored and
// already decided this unit may use. Writing it that way rather than as a URL
// is what keeps a page portable: the API path a figure is served from is a
// deployment detail, and a body full of /api/v1/... links would have to be
// rewritten every time that changed. It also makes the reference checkable --
// an id either belongs to this unit's source or it does not, which is the same
// question ValidatePage already asks about wikilinks.

// FigureScheme is the URL scheme a page uses to cite a stored figure.
const FigureScheme = "figure:"

// figureRef matches a markdown image whose destination is a figure reference.
// The caption is captured because an empty one is worth reporting: a picture
// with no words is a picture a reader cannot navigate by.
var figureRef = regexp.MustCompile(`!\[([^\]]*)\]\(figure:([^)\s]+)\)`)

// FigureRefs returns the figure ids a body cites, in order of appearance and
// without duplicates.
func FigureRefs(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range figureRef.FindAllStringSubmatch(body, -1) {
		id := strings.TrimSpace(m[2])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// checkFigures rejects references to figures this page is not entitled to use.
//
// An unknown id is always a mistake worth failing on, and it is a *visible*
// one: unlike a dangling wikilink, which renders as a marked gap the wiki
// treats as a request, a broken image is just broken. There is no useful
// half-state, so unlike links there is no tolerance count here.
func checkFigures(p *Page, allowed map[string]bool) []Violation {
	refs := FigureRefs(p.Body)
	if len(refs) == 0 {
		return nil
	}

	var unknown []string
	for _, id := range refs {
		if !allowed[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return []Violation{{Path: p.Path, Reason: fmt.Sprintf(
		"references %d figure(s) that do not belong to this page's source: %s; "+
			"use only the IDs listed for this unit",
		len(unknown), strings.Join(unknown, ", "))}}
}

// ResolveFigures rewrites figure references into servable URLs.
//
// Done at read time rather than at write time so the stored body stays
// portable: the same page renders correctly behind a different mount point, or
// in an export that resolves figures somewhere else entirely.
func ResolveFigures(body string, url func(id string) string) string {
	return figureRef.ReplaceAllStringFunc(body, func(m string) string {
		parts := figureRef.FindStringSubmatch(m)
		if parts == nil {
			return m
		}
		resolved := url(strings.TrimSpace(parts[2]))
		if resolved == "" {
			return m
		}
		return "![" + parts[1] + "](" + resolved + ")"
	})
}
