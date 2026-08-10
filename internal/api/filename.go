package api

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Uploaded files arrive named by whatever produced them, and what produced them
// was usually a scanner, an export button, or a person on their fourth revision:
// "NBAE5150_syllabus_v3_FINAL.pdf", "quarterly-report-2026-03-14.pdf",
// "doc-8f3a9c2b.docx". Listed verbatim, a bench's sources become a column of
// version suffixes and timestamps that all look alike.
//
// So a display name is derived on the way in, and the filename it came from is
// kept beside it -- the row still knows exactly which upload it is, which
// matters when someone re-uploads a corrected copy and needs to find the one
// they replaced.
//
// Derivation is deterministic and free. It reads the filename, nothing else: an
// LLM could title these better by opening them, but that spends money on every
// upload and makes the name depend on a model version. The pipeline already
// titles the *page* a document produces from its contents; this is the label on
// the raw material, and it has to be right before a build has ever run.

var (
	// Separators people use where a space belongs.
	nameSeparators = regexp.MustCompile(`[_\-.+\s]+`)
	// A trailing "(1)", "(2)" from a browser's duplicate-download naming.
	nameDupSuffix = regexp.MustCompile(`\s*\(\d+\)\s*$`)
	// "v3", "v2.1" -- a version token, not a word. (The "2.1" arrives split by
	// the separator pass, which is why only the head is matched here.)
	nameVersionToken = regexp.MustCompile(`^v\d+$`)
	// A content hash or opaque id: long, hex, and containing at least one
	// letter, so a run of digits that might be a real number is not swept up.
	nameHashToken = regexp.MustCompile(`(?i)^[0-9a-f]{8,}$`)
	nameHasLetter = regexp.MustCompile(`[a-zA-Z]`)
	// A four-digit year in the range anyone uploads documents from.
	nameYear   = regexp.MustCompile(`^(19|20)\d{2}$`)
	nameDigits = regexp.MustCompile(`^\d+$`)
	// Revision noise, as its own word. Deliberately short: "scan", "doc" and
	// "report" are often the only descriptive thing in a name, and dropping
	// them leaves a label made of nothing.
	nameNoise = map[string]bool{
		"final": true, "draft": true, "copy": true, "latest": true,
		"updated": true, "revised": true, "version": true, "ver": true,
		"rev": true, "new": true, "old": true,
	}
	// Words that stay lowercase inside a title.
	nameMinorWords = map[string]bool{
		"a": true, "an": true, "and": true, "as": true, "at": true, "but": true,
		"by": true, "for": true, "from": true, "in": true, "of": true, "on": true,
		"or": true, "the": true, "to": true, "vs": true, "with": true,
	}
)

// descriptiveName turns an uploaded file's name into something worth reading in
// a list. It never returns empty: a name made entirely of noise keeps the
// filename's stem, because a blank label is worse than an ugly one.
//
// It is a label, not an identifier. Two uploads can derive the same one -- that
// is the point of dropping "v2" -- so nothing downstream may key on it. The
// path remains what the row is unique on.
func descriptiveName(name string) string {
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	stem := strings.TrimSuffix(base, path.Ext(base))
	if strings.TrimSpace(stem) == "" {
		// A dotfile ("​.pdf") is all extension; there is nothing else to show.
		stem = base
	}
	fallback := strings.TrimSpace(stem)

	tokens := nameSeparators.Split(nameDupSuffix.ReplaceAllString(stem, ""), -1)

	var kept []string
	for _, w := range tokens {
		if w == "" {
			continue
		}
		lower := strings.ToLower(w)
		if nameNoise[lower] || nameVersionToken.MatchString(lower) {
			continue
		}
		if nameHashToken.MatchString(w) && nameHasLetter.MatchString(w) {
			continue
		}
		kept = append(kept, w)
	}
	kept = dropStamps(kept)
	if len(kept) == 0 {
		return fallback
	}
	return titleCase(kept)
}

// dropStamps removes the date and time fragments a scanner or an export leaves
// behind, which arrive as runs of bare numbers once the separators are gone:
// "quarterly report 2026 03 14" is a report, and the date is the part nobody
// reads.
//
// A lone number survives unless it is a year, because it is usually load-bearing
// -- "form 1099", "route 66", "section 8". Two or more in a row is a timestamp,
// whatever the numbers happen to be.
func dropStamps(tokens []string) []string {
	stampish := make([]bool, len(tokens))
	for i, w := range tokens {
		stampish[i] = nameDigits.MatchString(w)
	}
	out := make([]string, 0, len(tokens))
	for i, w := range tokens {
		if stampish[i] {
			runs := (i > 0 && stampish[i-1]) || (i+1 < len(tokens) && stampish[i+1])
			if runs || nameYear.MatchString(w) {
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

// titleCase capitalizes the words a title capitalizes, and leaves alone the
// ones that arrived already shaped -- an acronym, a course code, "McClean",
// "iPhone": anything with an interior capital its author meant.
func titleCase(words []string) string {
	out := make([]string, 0, len(words))
	for i, w := range words {
		switch {
		case hasInteriorUpper(w):
		case i > 0 && i < len(words)-1 && nameMinorWords[strings.ToLower(w)]:
			w = strings.ToLower(w)
		default:
			w = upperFirst(strings.ToLower(w))
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

func hasInteriorUpper(w string) bool {
	r := []rune(w)
	for i := 1; i < len(r); i++ {
		if unicode.IsUpper(r[i]) {
			return true
		}
	}
	return false
}

func upperFirst(w string) string {
	r := []rune(w)
	if len(r) == 0 {
		return w
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
