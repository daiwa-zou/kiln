// Inputs for the characterization fixtures. Synthetic throughout: invented
// pages about an invented kiln workshop, written to hit the renderer's
// branches, never copied from a real wiki. `npm run fixtures` runs each input
// through the legacy app.js and writes input + output to the JSON beside this
// file; the tests only compare against that JSON.
import type { PageSummary } from "../legacy";

// The clock every fixture is generated and checked at, so "3 days ago" in a
// freshness reason is stable.
export const NOW = "2026-06-15T12:00:00Z";

// The bench the fixtures render against: wikilinks resolve to these titles,
// nearestPages ranks them, and freshness measures against REFS.
export const PAGES: PageSummary[] = [
  { slug: "glaze-chemistry", title: "Glaze chemistry", type: "concept", builtAtRef: "r3", updated: "2026-06-14" },
  { slug: "kiln-firing-schedule", title: "Kiln firing schedule", type: "concept", builtAtRef: "r2", updated: "2026-06-01" },
  { slug: "cone-ratings", title: "Cone ratings", type: "entity", builtAtRef: "r1" },
  { slug: "reduction-firing", title: "Reduction firing", type: "concept", builtAtRef: "r0", updated: "2026-05-20T08:30:00Z" },
  { slug: "studio-safety", title: "Studio safety", type: "synthesis" },
  { slug: "clay-bodies", title: "Clay bodies", type: "source", builtAtRef: "r3" },
  { slug: "acquisitions", title: "Equipment acquisitions", type: "query", builtAtRef: "r3" },
  { slug: "kiln-wash", title: "", type: "entity", builtAtRef: "r3" },
];

// Newest first, as the run feed orders them.
export const REFS = ["r3", "r2", "r1"];

export const MARKDOWN: { name: string; src: string; headingOffset?: number }[] = [
  { name: "headings at page offset", src: "# Top\n\n## Section\n\n### Sub\n\nText under it." },
  { name: "headings at artifact offset", src: "# Title\n\n## Part\n\nBody.", headingOffset: 0 },
  { name: "frontmatter is stripped", src: "---\ntitle: Glaze\ntype: concept\n---\n# Glaze\n\nBody text." },
  { name: "hard-wrapped paragraph joins", src: "A paragraph hard wrapped\nat a narrow column\nstays one paragraph.\n\nA second one." },
  { name: "emphasis and code", src: "Some *emphasis*, **strong**, ~~struck~~ and `inline code`." },
  { name: "live wikilink shows title", src: "See [[glaze-chemistry]] for the oxides." },
  { name: "aliased wikilink", src: "See [[glaze-chemistry|the glaze page]]." },
  { name: "dead wikilink keeps slug", src: "See [[no-such-page]] and [[Another Missing]]." },
  { name: "wikilink to untitled page", src: "Use [[kiln-wash]] on shelves." },
  { name: "unordered list", src: "- one\n- two with [[cone-ratings]]\n- three" },
  { name: "ordered list", src: "1. first\n2. second\n3. third" },
  { name: "list then paragraph", src: "- a\n- b\n\nAfter the list." },
  { name: "table", src: "| Cone | Temp |\n|---|---|\n| 06 | 999 C |\n| 6 | 1222 C |" },
  { name: "fenced code", src: "```\nconst x = 1 < 2;\n```\n\nAfter." },
  { name: "blockquote", src: "> A quoted line\n> and another." },
  { name: "horizontal rule", src: "Above.\n\n---\n\nBelow." },
  { name: "markdown link", src: "Read [the docs](https://docs.example.org/glaze) and [local](#/page/clay-bodies)." },
  { name: "off-origin image becomes a link", src: "![Firing curve](https://cdn.example.net/curve.png?ref=wiki)" },
  { name: "off-origin image without alt", src: "![](https://cdn.example.net/raw.png)" },
  { name: "same-origin image", src: "![Kiln](/api/v1/workspaces/w/figures/abc.png)" },
  { name: "data image", src: "![dot](data:image/png;base64,iVBORw0KGgo=)" },
  { name: "single citation", src: "Cone 6 is about 1222 C (notes/cones.md:4-9)." },
  { name: "en dash citation", src: "Bisque first (notes/firing.md:1–8)." },
  { name: "multi-file citation", src: "Both agree (a.md:3, b.md:4-5, 7)." },
  { name: "repeated citation numbers once", src: "First (x.md:1). Again (x.md:2). Other (y.md:3)." },
  { name: "citation inside code span stays code", src: "Literal `(x.md:1)` here, and `y.md:4` as a cite." },
  { name: "citation lead-in word", src: "This holds (see glazes/notes.md:10-12)." },
  { name: "parenthetical that is not a citation", src: "Hot (very hot, really) kilns." },
  { name: "heading with citation", src: "## Firing (notes/firing.md:2)\n\nBody." },
  { name: "hostile script tag", src: "<script>alert(1)</script> and <img src=x onerror=alert(1)>" },
  { name: "hostile javascript link", src: "[click](javascript:alert(1)) and [x](JaVaScRiPt:alert(2))" },
  { name: "hostile attribute break in link", src: "[a](https://e.example/\"onmouseover=\"alert(1)) [[x\" onclick=\"y]]" },
  { name: "hostile image url", src: "![x](javascript:alert(1)) ![y](https://e.example/a.png\"onerror=\"alert(1))" },
  { name: "hostile wikilink alias", src: "[[glaze-chemistry|<b onmouseover=alert(1)>bold</b>]]" },
  { name: "hostile entities", src: "&lt;script&gt; &amp; &#x3C;svg/onload=alert(1)&#x3E;" },
  { name: "empty body", src: "" },
];

export const DECKS: { name: string; body: string }[] = [
  { name: "deck present", body: "Glaze chemistry balances fluxes, stabilizers and glass formers. The rest follows.\n\n## Oxides\n\nMore." },
  { name: "deck with citation", body: "Cone 6 glazes mature near 1222 degrees Celsius (notes/cones.md:4-9). Details below." },
  { name: "deck with wikilink and link", body: "A [[reduction-firing]] starves the kiln of oxygen to shift [colour](https://e.example/c) in iron glazes." },
  { name: "deck under heading only", body: "# Firing\n\nA firing schedule sets the rate the kiln climbs and holds.\n\n## Ramp\n\nDetail." },
  { name: "deck absent: opens with list", body: "- first item\n- second item\n\nThen prose." },
  { name: "deck absent: opens with table", body: "| a | b |\n|---|---|\n| 1 | 2 |" },
  { name: "deck absent: sentence too short", body: "Too short. Then more text follows here." },
  { name: "deck absent: sentence too long", body: `${"A very long sentence that keeps going ".repeat(8)}until it ends.` },
  { name: "deck absent: abbreviation is not an end", body: "Cones e.g. six and ten are common in studios that fire stoneware" },
  { name: "empty body", body: "" },
  { name: "frontmatter then deck", body: "---\ntitle: X\n---\nClay bodies differ in plasticity, shrinkage and firing range.\n\nMore." },
];

export const FLATTEN: { name: string; sentence: string }[] = [
  { name: "citations removed", sentence: "Cone 6 is hot (notes/cones.md:4-9)." },
  { name: "wikilinks to titles", sentence: "See [[glaze-chemistry]] and [[missing-one]] and [[cone-ratings|cones]]." },
  { name: "emphasis dropped", sentence: "A **strong** and *soft* ~~gone~~ claim." },
  { name: "code-span citation", sentence: "Stated in `notes/a.md:3` already." },
];

export const PARSE_CITES: string[] = [
  "notes/cones.md:4-9",
  "notes/firing.md:1–8",
  "a.md:3, b.md:4-5, 7",
  "see glazes/notes.md:10-12",
  "path with spaces.md:2",
  "Source: x.md:1",
  "x.md",
  "very hot, really",
  "x.md:",
  "cf. y.md:3",
  "",
];

export const FIND_CITES: string[] = [
  "One (a.md:1) and two (b.md:2-3).",
  "Nested ((a.md:1)) parens.",
  "Code `(a.md:1)` then (b.md:2).",
  "A [link](x.md:1) is not a cite (c.md:9).",
  "No groups here.",
  "Unclosed (a.md:1",
];

export const NEAREST: { slug: string; limit: number }[] = [
  { slug: "glaze", limit: 5 },
  { slug: "kiln-firing", limit: 5 },
  { slug: "acquisition", limit: 3 },
  { slug: "firing-schedule-for-cone-six", limit: 5 },
  { slug: "zzz-nothing-matches", limit: 5 },
  { slug: "clay", limit: 1 },
];

// One page per freshness state, plus the no-refs case.
export const FRESHNESS: { name: string; page: PageSummary | null; refs?: string[] }[] = [
  { name: "fired: built at the newest ref", page: { slug: "a", title: "A", builtAtRef: "r3" } },
  { name: "cooling: one build behind", page: { slug: "b", title: "B", builtAtRef: "r2" } },
  { name: "cooling: two builds behind", page: { slug: "c", title: "C", builtAtRef: "r1" } },
  { name: "stale: ref older than the feed, dated", page: { slug: "d", title: "D", builtAtRef: "r0", updated: "2026-06-12" } },
  { name: "stale: ref older than the feed, undated", page: { slug: "e", title: "E", builtAtRef: "r0" } },
  { name: "unknown: no ref on the page", page: { slug: "f", title: "F" } },
  { name: "unknown: no runs in the feed", page: { slug: "g", title: "G", builtAtRef: "r3" }, refs: [] },
  { name: "unknown: no page", page: null },
];
