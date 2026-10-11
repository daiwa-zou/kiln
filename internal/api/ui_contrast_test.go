package api

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The palette's WCAG 2.2 AA contrast, asserted from the theme tokens in
// style.css for both themes. docs/accessibility.md records the audit that
// set these values; this keeps a token edit from quietly undoing it. It
// checks token pairs, not rendered pixels: opacity, overlap, and which pair a
// rule actually uses remain the manual audit's job.

// uiThemes returns the light tokens and the dark tokens (dark merged over
// light, as the cascade does), keyed by name without the leading "--".
func uiThemes(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(uiSource(t, "style.css"), "")
	decls := func(block string) map[string]string {
		out := map[string]string{}
		for _, m := range regexp.MustCompile(`--([a-z0-9-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(block, -1) {
			out[m[1]] = strings.TrimSpace(m[2])
		}
		return out
	}
	lm := regexp.MustCompile(`(?s)^[^{]*?:root\s*\{([^}]*)\}`).FindStringSubmatch(css)
	dm := regexp.MustCompile(`(?s)@media\s*\(prefers-color-scheme:\s*dark\)\s*\{\s*:root\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if lm == nil || dm == nil {
		t.Fatal("style.css: could not find the :root token block and its prefers-color-scheme: dark override")
	}
	light = decls(lm[1])
	dark = map[string]string{}
	for k, v := range light {
		dark[k] = v
	}
	for k, v := range decls(dm[1]) {
		dark[k] = v
	}
	return light, dark
}

// luminance is WCAG relative luminance of a #rgb or #rrggbb colour.
func luminance(hex string) (float64, error) {
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 || !strings.HasPrefix(hex, "#") {
		return 0, fmt.Errorf("%q is not a #rgb or #rrggbb colour", hex)
	}
	var ch [3]float64
	for i := range ch {
		v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return 0, fmt.Errorf("%q: %w", hex, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2], nil
}

func contrast(a, b string) (float64, error) {
	la, err := luminance(a)
	if err != nil {
		return 0, err
	}
	lb, err := luminance(b)
	if err != nil {
		return 0, err
	}
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05), nil
}

func TestUIThemeContrast(t *testing.T) {
	type pair struct{ fg, bg string }
	var text, ui []pair
	on := func(dst *[]pair, fg string, bgs ...string) {
		for _, bg := range bgs {
			*dst = append(*dst, pair{fg, bg})
		}
	}
	// Text: 4.5:1 (WCAG 1.4.3).
	on(&text, "ink", "bg", "panel", "surface", "sunk")
	on(&text, "ink-dim", "bg", "panel", "surface", "sunk")
	// Not on --sunk: 4.34 light / 4.16 dark, recorded in accessibility.md as
	// deliberate -- no text is set inside a trough.
	on(&text, "ink-faint", "bg", "panel", "surface")
	on(&text, "ember", "bg", "panel")
	on(&text, "ember-text", "surface", "panel", "bg")
	on(&text, "on-ember", "ember")
	on(&text, "on-oxide", "oxide")
	on(&text, "oxide", "bg", "panel")
	for _, kind := range []string{"contradiction", "uncertain", "deletion", "gap"} {
		on(&text, "kind-"+kind, "kind-"+kind+"-bg", "panel", "bg")
	}
	// Non-text: 3:1 (WCAG 1.4.11) -- focus rings and links' ember, and the
	// type colours that mark graph nodes and tree dots.
	on(&ui, "ember", "bg", "panel")
	for _, typ := range []string{"concept", "entity", "source", "synthesis", "query", "comparison"} {
		on(&ui, "type-"+typ, "bg")
	}
	// --border, --border-soft and --stale are decorative: nothing is
	// conveyed by them alone, so they carry no ratio.

	light, dark := uiThemes(t)
	for _, theme := range []struct {
		name   string
		tokens map[string]string
	}{{"light", light}, {"dark", dark}} {
		check := func(pairs []pair, floor float64) {
			for _, p := range pairs {
				fg, okf := theme.tokens[p.fg]
				bg, okb := theme.tokens[p.bg]
				if !okf || !okb {
					t.Errorf("%s: token --%s or --%s is missing from style.css", theme.name, p.fg, p.bg)
					continue
				}
				r, err := contrast(fg, bg)
				if err != nil {
					t.Errorf("%s: --%s on --%s: %v", theme.name, p.fg, p.bg, err)
					continue
				}
				if r < floor {
					t.Errorf("%s: --%s %s on --%s %s is %.2f:1, under %.1f:1", theme.name, p.fg, fg, p.bg, bg, r, floor)
				}
			}
		}
		check(text, 4.5)
		check(ui, 3)
	}
}

func TestContrastMath(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want float64
	}{
		{"#000", "#fff", 21},
		{"#ffffff", "#ffffff", 1},
		{"#777", "#fff", 4.48},
	} {
		got, err := contrast(tc.a, tc.b)
		if err != nil || math.Abs(got-tc.want) > 0.01 {
			t.Errorf("contrast(%s, %s) = %.3f, %v; want %.2f", tc.a, tc.b, got, err, tc.want)
		}
	}
	for _, bad := range []string{"red", "#12", "#gggggg", "rgb(0,0,0)", "var(--ink)"} {
		if _, err := luminance(bad); err == nil {
			t.Errorf("luminance(%q) accepted an unparseable colour", bad)
		}
	}
}
