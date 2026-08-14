package extract

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The fixtures here are built rather than committed. A PDF with known images at
// known sizes is the only way to assert the decorative filter, and generating
// it keeps what is in the document visible in the test instead of hidden in a
// binary nobody can read in review.

// fixtureImage is one image placed into a generated PDF.
type fixtureImage struct {
	name          string
	width, height int
	page          int
	// tint varies the pixel data so two differently-named images do not hash
	// alike -- deduplication is one of the things under test.
	tint byte
}

func solidPixels(width, height int, tint byte) []byte {
	out := make([]byte, 0, width*height*3)
	for i := range width * height {
		out = append(out, tint+byte(i%7), tint+byte(i%5), tint+byte(i%3))
	}
	return out
}

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// writeFixturePDF builds a minimal but valid PDF embedding the given images,
// one text line per page, and returns its path.
func writeFixturePDF(t *testing.T, pages int, imgs []fixtureImage) string {
	t.Helper()

	var objs [][]byte
	add := func(b []byte) int { objs = append(objs, b); return len(objs) }

	add([]byte("<< /Type /Catalog /Pages 2 0 R >>"))
	add(nil) // page tree, backfilled once the page objects exist
	add([]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"))

	imageObj := map[string]int{}
	for _, im := range imgs {
		data := deflate(solidPixels(im.width, im.height, im.tint))
		var b bytes.Buffer
		fmt.Fprintf(&b, "<< /Type /XObject /Subtype /Image /Width %d /Height %d "+
			"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n",
			im.width, im.height, len(data))
		b.Write(data)
		b.WriteString("\nendstream")
		imageObj[im.name] = add(b.Bytes())
	}

	var pageObjs []int
	for p := 1; p <= pages; p++ {
		var content, xobjs bytes.Buffer
		fmt.Fprintf(&content, "BT /F1 12 Tf 60 760 Td (Figure %d: quarterly revenue by region) Tj ET\n", p)
		for _, im := range imgs {
			if im.page != p {
				continue
			}
			fmt.Fprintf(&content, "q 200 0 0 200 60 400 cm /%s Do Q\n", im.name)
			fmt.Fprintf(&xobjs, "/%s %d 0 R ", im.name, imageObj[im.name])
		}
		cs := content.Bytes()
		contentObj := add(fmt.Appendf(nil, "<< /Length %d >>\nstream\n%s\nendstream", len(cs), cs))
		pageObjs = append(pageObjs, add(fmt.Appendf(nil,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R "+
				"/Resources << /Font << /F1 3 0 R >> /XObject << %s>> >> >>",
			contentObj, xobjs.String())))
	}

	var kids bytes.Buffer
	for _, p := range pageObjs {
		fmt.Fprintf(&kids, "%d 0 R ", p)
	}
	objs[1] = fmt.Appendf(nil, "<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), len(pageObjs))

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, o := range objs {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(o)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)

	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writePNG emits a real PNG, for fixtures that go through a tool which will
// actually decode them.
func writePNG(t *testing.T, path string, width, height int, tint byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{
				R: tint + byte(x%7), G: tint + byte(y%5), B: tint + byte((x+y)%3), A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func requireTool(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s is not installed", binary)
	}
}

func refsOf(figs []Figure) []string {
	out := make([]string, 0, len(figs))
	for _, f := range figs {
		out = append(out, f.Ref)
	}
	return out
}

// TestPDFFiguresKeepContentAndDropFurniture is the judgment the whole package
// exists for: a report's chart and photograph are figures, its header logo and
// its divider rule are not.
func TestPDFFiguresKeepContentAndDropFurniture(t *testing.T) {
	requireTool(t, "pdfimages")
	requireTool(t, "pdftotext")

	path := writeFixturePDF(t, 2, []fixtureImage{
		{name: "Chart", width: 480, height: 320, page: 1, tint: 20},
		{name: "Logo", width: 24, height: 24, page: 1, tint: 90},
		{name: "Photo", width: 400, height: 300, page: 2, tint: 160},
		{name: "Rule", width: 300, height: 2, page: 2, tint: 10},
	})

	res, err := DefaultExtractors().ExtractWithFigures(
		context.Background(), path, DefaultFigureOptions())
	if err != nil {
		t.Fatalf("ExtractWithFigures: %v", err)
	}
	if res.FigureErr != nil {
		t.Fatalf("FigureErr: %v", res.FigureErr)
	}
	if len(res.Figures) != 2 {
		t.Fatalf("kept %d figures %v, want the chart and the photo", len(res.Figures), refsOf(res.Figures))
	}

	for _, f := range res.Figures {
		if f.Width < 64 || f.Height < 64 {
			t.Errorf("kept a figure below the minimum edge: %+v", f)
		}
		if len(f.Data) == 0 {
			t.Errorf("figure %s has no bytes", f.Ref)
		}
		if f.SHA256 == "" {
			t.Errorf("figure %s has no digest", f.Ref)
		}
		if f.ContentType != "image/png" {
			t.Errorf("figure %s content type = %q", f.Ref, f.ContentType)
		}
	}

	// Document order, so a reader and the model see them as the document did.
	if res.Figures[0].Page != 1 || res.Figures[1].Page != 2 {
		t.Errorf("figures are not in document order: %+v", res.Figures)
	}
	// The caption line on each page is attributed to that page's figure.
	if res.Figures[0].Caption != "Figure 1: quarterly revenue by region" {
		t.Errorf("caption = %q", res.Figures[0].Caption)
	}
}

// TestPDFFiguresDropImagesRepeatedAcrossPages: the same picture on every page
// is a letterhead, and no size rule catches it because letterheads are often
// large. Repetition is the signal.
func TestPDFFiguresDropImagesRepeatedAcrossPages(t *testing.T) {
	requireTool(t, "pdfimages")

	// The identical banner appears on four pages; the chart appears once.
	imgs := []fixtureImage{{name: "Chart", width: 480, height: 320, page: 2, tint: 20}}
	for p := 1; p <= 4; p++ {
		imgs = append(imgs, fixtureImage{
			name: fmt.Sprintf("Banner%d", p), width: 300, height: 120, page: p, tint: 77,
		})
	}
	path := writeFixturePDF(t, 4, imgs)

	res, err := DefaultExtractors().ExtractWithFigures(
		context.Background(), path, DefaultFigureOptions())
	if err != nil {
		t.Fatalf("ExtractWithFigures: %v", err)
	}
	if len(res.Figures) != 1 {
		t.Fatalf("kept %d figures %v, want only the chart", len(res.Figures), refsOf(res.Figures))
	}
	if res.Figures[0].Page != 2 {
		t.Errorf("kept the wrong figure: %+v", res.Figures[0])
	}
}

// TestExtractWithoutFigureOptionsCostsNothing: the zero value must not start
// running image extraction for every existing caller.
func TestExtractWithoutFigureOptionsCostsNothing(t *testing.T) {
	requireTool(t, "pdfimages")

	path := writeFixturePDF(t, 1, []fixtureImage{
		{name: "Chart", width: 480, height: 320, page: 1, tint: 20},
	})

	res, err := DefaultExtractors().ExtractWithFigures(context.Background(), path, FigureOptions{})
	if err != nil {
		t.Fatalf("ExtractWithFigures: %v", err)
	}
	if len(res.Figures) != 0 {
		t.Errorf("zero options produced %d figures", len(res.Figures))
	}

	plain, err := DefaultExtractors().Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(plain.Figures) != 0 {
		t.Errorf("Extract produced %d figures", len(plain.Figures))
	}
}

// TestOfficeFiguresCarryAuthoredCaptions: the alt text an author wrote is the
// best description a figure ever gets, and pandoc hands it over.
func TestOfficeFiguresCarryAuthoredCaptions(t *testing.T) {
	requireTool(t, "pandoc")

	dir := t.TempDir()
	// Built through pandoc so the docx is a real one; a hand-rolled zip would
	// be testing this package against a fixture only it understands.
	chart := filepath.Join(dir, "chart.png")
	logo := filepath.Join(dir, "logo.png")
	writePNG(t, chart, 480, 320, 20)
	writePNG(t, logo, 24, 24, 90)

	md := filepath.Join(dir, "doc.md")
	body := "# Handbook\n\nIntro.\n\n![Revenue by quarter](" + chart + ")\n\nMore.\n\n![](" + logo + ")\n"
	if err := os.WriteFile(md, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	docx := filepath.Join(dir, "doc.docx")
	if out, err := exec.Command("pandoc", md, "-o", docx).CombinedOutput(); err != nil {
		t.Skipf("pandoc could not build the fixture docx: %v (%s)", err, out)
	}

	res, err := DefaultExtractors().ExtractWithFigures(
		context.Background(), docx, DefaultFigureOptions())
	if err != nil {
		t.Fatalf("ExtractWithFigures: %v", err)
	}
	if res.FigureErr != nil {
		t.Fatalf("FigureErr: %v", res.FigureErr)
	}
	if len(res.Figures) != 1 {
		t.Fatalf("kept %d figures, want only the captioned chart: %+v", len(res.Figures), res.Figures)
	}
	if res.Figures[0].Caption != "Revenue by quarter" {
		t.Errorf("caption = %q, want the author's alt text", res.Figures[0].Caption)
	}
	if res.Figures[0].Width != 480 || res.Figures[0].Height != 320 {
		t.Errorf("dimensions = %dx%d", res.Figures[0].Width, res.Figures[0].Height)
	}
}

func TestFigureOptionsKeep(t *testing.T) {
	o := DefaultFigureOptions()
	tests := []struct {
		name          string
		width, height int
		want          bool
	}{
		{"chart", 480, 320, true},
		{"small but real chart", 200, 160, true},
		{"icon", 24, 24, false},
		{"bullet glyph", 8, 8, false},
		{"horizontal rule", 600, 2, false},
		{"narrow sidebar strip", 40, 700, false},
		{"banner at the aspect limit", 720, 60, false},
		{"wide chart within limits", 600, 120, true},
		{"tall infographic", 400, 1600, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := o.keep(tt.width, tt.height); got != tt.want {
				t.Errorf("keep(%d, %d) = %v, want %v", tt.width, tt.height, got, tt.want)
			}
		})
	}
}

// TestSelectFiguresCapsAndOrders exercises the whole-set rules without needing
// any tool installed.
func TestSelectFiguresCapsAndOrders(t *testing.T) {
	o := DefaultFigureOptions()
	o.Max = 2

	figs := []Figure{
		{Ref: "p1-i0", Page: 1, Width: 100, Height: 100, Data: []byte("a"), SHA256: "a"},
		{Ref: "p2-i1", Page: 2, Width: 400, Height: 400, Data: []byte("b"), SHA256: "b"},
		{Ref: "p3-i2", Page: 3, Width: 300, Height: 300, Data: []byte("c"), SHA256: "c"},
		// An exact duplicate of the first, on the same page: collapsed, not
		// treated as furniture, because it is not spread across pages.
		{Ref: "p1-i3", Page: 1, Width: 100, Height: 100, Data: []byte("a"), SHA256: "a"},
	}

	got := selectFigures(figs, o)
	if len(got) != 2 {
		t.Fatalf("kept %d figures %v, want 2", len(got), refsOf(got))
	}
	// The cap takes the largest, then presentation restores document order.
	if got[0].Ref != "p2-i1" || got[1].Ref != "p3-i2" {
		t.Errorf("kept %v, want the two largest in page order", refsOf(got))
	}
}

func TestSelectFiguresRespectsByteBudget(t *testing.T) {
	o := DefaultFigureOptions()
	o.MaxBytes = 10

	got := selectFigures([]Figure{
		{Ref: "big", Page: 1, Width: 400, Height: 400, Data: make([]byte, 8), SHA256: "a"},
		{Ref: "also-big", Page: 2, Width: 300, Height: 300, Data: make([]byte, 8), SHA256: "b"},
	}, o)

	if len(got) != 1 || got[0].Ref != "big" {
		t.Errorf("kept %v, want only the first within budget", refsOf(got))
	}
}

// TestUnderMediaRefusesEscapes: the <img src> in a converted document is
// attacker-controlled text. Anything resolving outside the extraction
// directory must read as "not a figure" rather than as a file to open.
func TestUnderMediaRefusesEscapes(t *testing.T) {
	media := filepath.Join(t.TempDir(), "media")

	for _, src := range []string{
		"../../../etc/passwd",
		"/etc/passwd",
		"https://example.com/logo.png",
		"http://example.com/logo.png",
		"media/../../../../etc/shadow",
	} {
		if _, _, ok := underMedia(media, src); ok {
			t.Errorf("underMedia accepted %q", src)
		}
	}

	// The ordinary case still resolves.
	if _, rel, ok := underMedia(media, filepath.Join(media, "media", "rId9.png")); !ok || rel == "" {
		t.Errorf("underMedia rejected a path inside the media directory")
	}
}

func TestPDFCaptions(t *testing.T) {
	text := "Some prose.\nFigure 1: Revenue by region\nmore prose\f" +
		"Second page intro\nFig. 2  Latency distribution\n\f" +
		"A page with no caption at all\n"

	got := pdfCaptions(text)
	if got[1] != "Figure 1: Revenue by region" {
		t.Errorf("page 1 caption = %q", got[1])
	}
	if got[2] != "Fig. 2 Latency distribution" {
		t.Errorf("page 2 caption = %q", got[2])
	}
	if _, ok := got[3]; ok {
		t.Errorf("page 3 invented a caption: %q", got[3])
	}
}
