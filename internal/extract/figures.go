package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	// Registered for image.DecodeConfig, which is how a figure's real pixel
	// dimensions are read. Dimensions are the whole basis of the decorative
	// filter, so a format that cannot be measured cannot be judged.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// Figures are the pictures and graphs a document carries, recovered so the wiki
// can show them rather than describe them from their captions.
//
// The hard part is not extraction, it is judgment. A real document's images are
// mostly not figures: header logos, bullet glyphs, rule lines, spacer pixels,
// signature scans. Including those makes a wiki page worse, so everything here
// is built around discarding them -- see keep() for the rules and why each one
// exists.
//
// KNOWN LIMITATION: this recovers *raster* images. A chart drawn with vector
// operators -- which is what Excel, Illustrator and matplotlib's PDF backend
// produce -- is not an embedded image and is not recovered. Scanned documents,
// screenshots, photographs and charts exported as PNG/JPEG all are. Rendering
// whole pages would catch the vector case and is deliberately not done here: a
// page render is the page, text and all, not the figure on it.

// Figure is one image recovered from a document.
type Figure struct {
	// Ref identifies the figure within its document: "p3-i2" for PDFs,
	// the media basename for Office documents. Stable across re-extraction of
	// unchanged bytes, which is what lets a page keep referring to it.
	Ref string
	// Page is the 1-based page it appeared on, or 0 for formats without pages.
	Page int
	// Width and Height are pixels.
	Width, Height int
	// Caption is the document's own words for this figure -- alt text, a
	// figcaption, or a "Figure 3: ..." line found near it. Empty when the
	// document never named it.
	Caption string
	// ContentType is the media type of Data.
	ContentType string
	// Data is the image itself.
	Data []byte
	// SHA256 digests Data, so the same picture ingested twice is one figure.
	SHA256 string
}

// Pixels is the figure's area, the primary size signal.
func (f Figure) Pixels() int { return f.Width * f.Height }

// FigureOptions bounds figure extraction. The zero value disables it, so a
// caller that has not thought about figures does not silently start paying for
// them.
type FigureOptions struct {
	// Max figures kept per document. Zero disables extraction entirely.
	Max int
	// MinEdge is the smallest allowed width or height in pixels.
	MinEdge int
	// MinPixels is the smallest allowed area.
	MinPixels int
	// MaxAspect is the widest allowed ratio of long edge to short edge.
	MaxAspect float64
	// MaxBytes caps the total size of the figures kept from one document.
	MaxBytes int64
	// RepeatPageLimit is how many distinct pages one identical image may
	// appear on before it is treated as page furniture rather than content.
	RepeatPageLimit int
}

// DefaultFigureOptions are tuned to keep charts, diagrams, screenshots and
// photographs while dropping the furniture. The numbers are deliberately
// conservative: a wrongly dropped figure is invisible, a wrongly kept logo is
// on the page for everyone to see.
func DefaultFigureOptions() FigureOptions {
	return FigureOptions{
		Max: 24,
		// 64px on an edge is below any figure worth showing and above every
		// icon, bullet and spacer.
		MinEdge: 64,
		// 20,000px is roughly 160x125 -- a small chart still clears it.
		MinPixels: 20_000,
		// 12:1 catches rules, dividers and banner strips. Wide-format charts
		// rarely pass 6:1, so this leaves real headroom.
		MaxAspect: 12,
		MaxBytes:  32 << 20,
		// Twice is a coincidence; three pages is a template.
		RepeatPageLimit: 3,
	}
}

// Enabled reports whether these options ask for any figures at all.
func (o FigureOptions) Enabled() bool { return o.Max > 0 }

// withDefaults fills unset bounds so a caller can set Max alone.
func (o FigureOptions) withDefaults() FigureOptions {
	d := DefaultFigureOptions()
	if o.MinEdge == 0 {
		o.MinEdge = d.MinEdge
	}
	if o.MinPixels == 0 {
		o.MinPixels = d.MinPixels
	}
	if o.MaxAspect == 0 {
		o.MaxAspect = d.MaxAspect
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = d.MaxBytes
	}
	if o.RepeatPageLimit == 0 {
		o.RepeatPageLimit = d.RepeatPageLimit
	}
	return o
}

// keep reports whether one candidate is plausibly a figure rather than
// furniture, on dimensions alone. Called before any bytes are read where the
// format allows it, because the cheapest image to discard is one never
// extracted.
func (o FigureOptions) keep(width, height int) bool {
	if width < o.MinEdge || height < o.MinEdge {
		return false
	}
	if width*height < o.MinPixels {
		return false
	}
	long, short := width, height
	if short > long {
		long, short = short, long
	}
	return short != 0 && float64(long)/float64(short) <= o.MaxAspect
}

// FigureExtractor is the optional half of Extractor. An extractor that
// implements it can also recover the document's images; one that does not
// simply contributes no figures, which is why the capability is a separate
// interface rather than four more methods every extractor must stub out.
type FigureExtractor interface {
	// ExtractFigures recovers images from a document. The text is passed in
	// because captions live in it -- pandoc writes alt text and figcaptions
	// into its markdown, and a PDF's captions are lines near the image.
	ExtractFigures(ctx context.Context, path, text string, o FigureOptions) ([]Figure, error)
}

// ExtractWithFigures normalizes one file and, when options ask for it and the
// chosen extractor supports it, recovers its figures too.
//
// Figure failures never fail the extraction. A document whose text was read
// fine is still worth a page; losing its pictures is a degradation, not an
// error, and treating it as one would mean a single malformed image costs the
// whole document.
func (es Extractors) ExtractWithFigures(ctx context.Context, path string, o FigureOptions) (*Result, error) {
	res, chosen, err := es.extract(ctx, path)
	if err != nil || !o.Enabled() {
		return res, err
	}
	fe, ok := chosen.(FigureExtractor)
	if !ok {
		return res, nil
	}

	// One return for both outcomes, because there is only one outcome as far
	// as the caller is concerned: the extraction succeeded. A figure failure is
	// recorded on the result rather than returned, so the two branches differ
	// in what they record and not in what they answer.
	figs, ferr := fe.ExtractFigures(ctx, path, res.Text, o.withDefaults())
	switch {
	case ferr != nil:
		res.FigureErr = ferr
	default:
		res.Figures = selectFigures(figs, o.withDefaults())
	}
	return res, nil
}

// selectFigures applies the rules that need the whole set rather than one
// image: drop what repeats across pages, collapse exact duplicates, then take
// the largest that fit the caps.
//
// Ordering matters. Repetition is judged before deduplication, because the
// evidence that something is furniture *is* the duplication -- collapsing
// first would erase the signal and promote every header logo to a figure.
func selectFigures(figs []Figure, o FigureOptions) []Figure {
	if len(figs) == 0 {
		return nil
	}

	pagesByHash := map[string]map[int]bool{}
	for _, f := range figs {
		if pagesByHash[f.SHA256] == nil {
			pagesByHash[f.SHA256] = map[int]bool{}
		}
		pagesByHash[f.SHA256][f.Page] = true
	}

	seen := map[string]bool{}
	var kept []Figure
	for _, f := range figs {
		if len(pagesByHash[f.SHA256]) >= o.RepeatPageLimit {
			continue
		}
		if seen[f.SHA256] {
			continue
		}
		seen[f.SHA256] = true
		kept = append(kept, f)
	}

	// Largest first, so the caps cut the least interesting images rather than
	// whichever happened to come last. Ties break on Ref for determinism: a
	// re-extraction that reorders figures would otherwise look like a change.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Pixels() != kept[j].Pixels() {
			return kept[i].Pixels() > kept[j].Pixels()
		}
		return kept[i].Ref < kept[j].Ref
	})

	var out []Figure
	var total int64
	for _, f := range kept {
		if len(out) >= o.Max || total+int64(len(f.Data)) > o.MaxBytes {
			break
		}
		total += int64(len(f.Data))
		out = append(out, f)
	}

	// Back into document order for presentation: the reader and the model both
	// want the figures in the order the document showed them.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Page != out[j].Page {
			return out[i].Page < out[j].Page
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readImage loads one extracted image file and measures it, returning ok=false
// for anything that is not a decodable image -- a converter can emit a stray
// file, and a figure that cannot be measured cannot be judged.
func readImage(path string) (data []byte, width, height int, contentType string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, "", false
	}
	cfg, format, err := image.DecodeConfig(strings.NewReader(string(data)))
	if err != nil {
		return nil, 0, 0, "", false
	}
	switch format {
	case "png":
		contentType = "image/png"
	case "jpeg":
		contentType = "image/jpeg"
	case "gif":
		contentType = "image/gif"
	default:
		return nil, 0, 0, "", false
	}
	return data, cfg.Width, cfg.Height, contentType, true
}

// --- PDF ---------------------------------------------------------------------

// pdfImagesBinary is poppler's image extractor, a sibling of pdftotext and
// present wherever it is.
func (p *PDFExtractor) figureBinary() string {
	if p.FigureBinary != "" {
		return p.FigureBinary
	}
	return "pdfimages"
}

// listPattern matches one row of `pdfimages -list`. Only the leading columns
// are addressed by position -- page, index, type, width, height -- because the
// trailing columns differ between poppler builds.
var listPattern = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(\S+)\s+(\d+)\s+(\d+)\s`)

// pdfCandidate is one row of the image inventory, before anything is extracted.
type pdfCandidate struct {
	page, index   int
	width, height int
}

// ExtractFigures recovers a PDF's embedded raster images.
//
// Two passes on purpose. `pdfimages -list` reports every image's dimensions
// without writing a byte, so the decorative filter runs before extraction and a
// document carrying four hundred bullet glyphs costs one subprocess and no
// disk. Only when something survives is the extraction run at all.
func (p *PDFExtractor) ExtractFigures(ctx context.Context, path, text string, o FigureOptions) ([]Figure, error) {
	listing, err := runTool(ctx, p.figureBinary(), p.Timeout, "-list", path)
	if err != nil {
		return nil, err
	}

	var wanted []pdfCandidate
	for _, line := range strings.Split(listing, "\n") {
		m := listPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Soft masks and stencil masks are the transparency channels of other
		// images, not images anyone wants to look at.
		if kind := m[3]; kind != "image" {
			continue
		}
		page, _ := strconv.Atoi(m[1])
		index, _ := strconv.Atoi(m[2])
		width, _ := strconv.Atoi(m[4])
		height, _ := strconv.Atoi(m[5])
		if !o.keep(width, height) {
			continue
		}
		wanted = append(wanted, pdfCandidate{page: page, index: index, width: width, height: height})
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	dir, err := os.MkdirTemp("", "kiln-figures-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// -p writes the page number into each filename, which is the only way to
	// map an extracted file back to the inventory row it came from.
	if _, err := runTool(ctx, p.figureBinary(), p.Timeout,
		"-png", "-p", path, filepath.Join(dir, "fig")); err != nil {
		return nil, err
	}

	byIndex := make(map[int]pdfCandidate, len(wanted))
	for _, c := range wanted {
		byIndex[c.index] = c
	}
	captions := pdfCaptions(text)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Figure
	for _, e := range entries {
		page, index, ok := parseFigureName(e.Name())
		if !ok {
			continue
		}
		cand, wantIt := byIndex[index]
		if !wantIt || cand.page != page {
			continue
		}
		data, w, h, ctype, ok := readImage(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		// Re-checked against the decoded file rather than trusted from the
		// listing: -png can convert a colorspace, and the bytes on disk are
		// what a reader will actually see.
		if !o.keep(w, h) {
			continue
		}
		out = append(out, Figure{
			Ref:         fmt.Sprintf("p%d-i%d", page, index),
			Page:        page,
			Width:       w,
			Height:      h,
			Caption:     captions[page],
			ContentType: ctype,
			Data:        data,
			SHA256:      hashBytes(data),
		})
	}
	return out, nil
}

// figureName matches pdfimages' "<prefix>-<page>-<index>.<ext>" output.
var figureName = regexp.MustCompile(`-(\d+)-(\d+)\.[a-z]+$`)

func parseFigureName(name string) (page, index int, ok bool) {
	m := figureName.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	page, _ = strconv.Atoi(m[1])
	index, _ = strconv.Atoi(m[2])
	return page, index, true
}

// captionLine matches the conventional opening of a figure caption.
var captionLine = regexp.MustCompile(`(?mi)^\s*((?:figure|fig\.?|chart|exhibit|plate|diagram)\s*\.?\s*\d+[.:)]?\s+\S.*)$`)

// pdfCaptions finds one caption per page from the extracted text.
//
// A PDF's images carry no alt text, so the document's own caption line is the
// only description available. Pages are separated by form feeds, which is what
// pdftotext emits, and the first caption-looking line on a page is attributed
// to that page's figure. Rough by design: a page with two figures gets one
// caption on both, which is still better than no words at all, and the model is
// told these are approximate.
func pdfCaptions(text string) map[int]string {
	out := map[int]string{}
	for i, page := range strings.Split(text, "\f") {
		if m := captionLine.FindStringSubmatch(page); m != nil {
			out[i+1] = strings.TrimSpace(collapseSpaces(m[1]))
		}
	}
	return out
}

var spaceRun = regexp.MustCompile(`\s+`)

func collapseSpaces(s string) string { return spaceRun.ReplaceAllString(s, " ") }

// --- Office ------------------------------------------------------------------

// imgTag matches the raw HTML pandoc emits for an image in gfm output.
var imgTag = regexp.MustCompile(`<img\s+src="([^"]+)"([^>]*)>`)
var altAttr = regexp.MustCompile(`alt="([^"]*)"`)

// ExtractFigures recovers the media an Office or ebook document embeds.
//
// pandoc does the work: --extract-media writes every embedded image to a
// directory and rewrites the text to point at it, so the same run that produces
// the prose produces the pictures, already associated with the alt text and
// captions the document's author wrote. That authored description is the
// strongest "is this a figure" signal available anywhere in this package --
// nobody writes alt text for a spacer.
func (p *PandocExtractor) ExtractFigures(ctx context.Context, path, _ string, o FigureOptions) ([]Figure, error) {
	dir, err := os.MkdirTemp("", "kiln-figures-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	media := filepath.Join(dir, "media")
	text, err := runTool(ctx, p.binary(), p.Timeout,
		"--sandbox", "--extract-media="+media, "-t", "gfm", "--wrap=none", path)
	if err != nil {
		return nil, err
	}

	var out []Figure
	for _, m := range imgTag.FindAllStringSubmatch(text, -1) {
		src, attrs := m[1], m[2]
		full, rel, ok := underMedia(media, src)
		if !ok {
			continue
		}

		data, w, h, ctype, ok := readImage(full)
		if !ok || !o.keep(w, h) {
			continue
		}
		caption := ""
		if a := altAttr.FindStringSubmatch(attrs); a != nil {
			caption = collapseSpaces(strings.TrimSpace(a[1]))
		}
		out = append(out, Figure{
			Ref:         mediaRef(rel),
			Width:       w,
			Height:      h,
			Caption:     caption,
			ContentType: ctype,
			Data:        data,
			SHA256:      hashBytes(data),
		})
	}
	return out, nil
}

// underMedia resolves an <img src> against the directory pandoc was told to
// extract into, and refuses anything that lands outside it.
//
// The containment check is the point. src comes out of a document written by
// someone else: pandoc emits an absolute path when --extract-media is
// absolute, a cwd-relative one otherwise, and a document that references
// ../../../etc/passwd or an http URL must read as "not a figure" rather than
// as a file to open.
func underMedia(media, src string) (full, rel string, ok bool) {
	if strings.Contains(src, "://") {
		return "", "", false
	}
	p := filepath.FromSlash(src)
	if !filepath.IsAbs(p) {
		// Relative to the process working directory, which is where pandoc
		// resolved it too.
		wd, err := os.Getwd()
		if err != nil {
			return "", "", false
		}
		p = filepath.Join(wd, p)
	}
	rel, err := filepath.Rel(media, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	return filepath.Join(media, rel), rel, true
}

// mediaRef turns a media-relative path into a stable identifier. pandoc names
// these from the document's own relationship ids, so they survive
// re-extraction of unchanged bytes.
func mediaRef(rel string) string {
	base := filepath.Base(rel)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
