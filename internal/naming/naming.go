// Package naming is kiln's convention for what an uploaded document is called.
//
// One convention, applied in two places that used to disagree. A file was
// named twice: once at upload from its filename alone, and again at ingest,
// where a document's own first heading was preferred but the fallback was the
// raw filename stem -- so "NBAE5150_syllabus_v3_FINAL.pdf" became "NBAE5150
// Syllabus" in the file list and "NBAE5150_syllabus_v3_FINAL" on the page it
// produced. Two answers to one question is not a convention.
//
// The convention:
//
//   - The document's own title when it has one -- its first top-level heading,
//     which is what the extractors emit for a PDF's title block, a Word
//     document's Title style, and a markdown file's "# ".
//   - Otherwise its filename, which is all that is left to go on.
//   - Either way: Title Case, minor words lowercase inside the title, machine
//     noise removed (extensions, version suffixes, duplicate-download markers,
//     timestamps, content hashes), whitespace collapsed, and a length cap so a
//     document that opens with a paragraph-long heading does not become a
//     paragraph-long name.
//   - Capitalisation its author chose is never overridden: "McClean", "iPhone"
//     and "NBAE5150" survive intact.
//
// Derivation is deterministic and free -- it reads the filename and the text
// already extracted for the build. An LLM would title these better, but it
// would spend a model call per document on something the document usually
// states in its own first line, and would make the name depend on a model
// version.
package naming

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

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

// FromFilename applies the convention to a filename alone. It is what upload
// time has to work with: the bytes are still streaming past and nothing has
// read them yet.
//
// It never returns empty -- a name made entirely of noise keeps the filename's
// stem, because a blank label is worse than an ugly one.
//
// It is a label, not an identifier. Two uploads can derive the same one -- that
// is the point of dropping "v2" -- so nothing downstream may key on it. The
// path remains what the row is unique on.
func FromFilename(name string) string {
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

// maxNameRunes caps a derived name. A heading is usually a title, but a
// document whose first heading is a sentence -- a memo that opens with its own
// subject line, a scanned form with a preamble at the top -- would otherwise
// put a paragraph in a column sized for a name.
const maxNameRunes = 90

// FromDocument applies the convention to a document whose text has been
// extracted: its own title if it states one, its filename if it does not.
//
// This is the ingest-time answer, and it is the better one, because by now the
// document has been read. A file called "scan_2026_03_14.pdf" whose first line
// is "Leadership Insights Syllabus (NBAE 5150, Fall 2026)" is that syllabus,
// and no amount of filename cleverness was ever going to discover it.
//
// The filename remains the fallback rather than a supplement. Appending it to
// a title the document already states produces "Q3 Board Memo (q3-board-memo)"
// -- the same words twice, which reads as a mistake. Where the filename says
// something the title does not, it is still on the row, one line below.
func FromDocument(filename, text string) string {
	if title := headingTitle(text); title != "" {
		if n := normalize(title); n != "" {
			return n
		}
	}
	return FromFilename(filename)
}

// headingTitle returns a document's first top-level heading.
//
// Only the first, and only "# ": the extractors emit a PDF's title block, a
// Word document's Title style and a markdown "# " all as one top-level
// heading, while "##" is a section within the document and naming the file
// after its first section would be worse than naming it after its filename.
//
// Bounded to the opening of the document. A title is at the top or it is not a
// title, and scanning a 300-page extraction to find a "#" on page 200 would
// name the file after a chapter.
func headingTitle(text string) string {
	const lookahead = 4 << 10
	if len(text) > lookahead {
		text = text[:lookahead]
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// normalize puts arbitrary prose through the convention: the same token
// filtering a filename gets, so a heading carrying a version marker or a
// trailing date is cleaned the same way, then capped.
//
// Punctuation inside a heading survives, unlike in a filename, where a dot is
// a separator. "Q3: Board Memo" is punctuated by its author; "q3.board.memo"
// is punctuated by a filesystem.
func normalize(title string) string {
	title = strings.TrimSpace(strings.Join(strings.Fields(title), " "))
	if title == "" {
		return ""
	}
	var kept []string
	for _, w := range strings.Fields(title) {
		lower := strings.ToLower(strings.Trim(w, ".,;:()[]"))
		if nameNoise[lower] || nameVersionToken.MatchString(lower) {
			continue
		}
		kept = append(kept, w)
	}
	kept = dropStamps(kept)
	if len(kept) == 0 {
		return ""
	}
	return truncateWords(titleCase(kept), maxNameRunes)
}

// truncateWords cuts on a word boundary and marks the cut, so a clipped name
// reads as clipped rather than as a document with an odd name.
func truncateWords(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	cut := string(r[:limit])
	if i := strings.LastIndex(cut, " "); i > limit/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "\u2026"
}
