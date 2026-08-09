# Accessibility

The reading UI targets **WCAG 2.2 level AA**. This records what that means
here, what was measured, and the two places the standard is met by a
deliberate exception rather than by the obvious rule.

## Audited

Measured in a browser against the running UI, across every view (overview,
index, graph, gaps, ingest, reviews, log, steering, members, page reader,
search), in both light and dark themes, at 1440px and at 375px:

| Check | Result |
| --- | --- |
| Text contrast ≥ 4.5:1 (3:1 for large text) | passes in both themes |
| Pointer targets ≥ 24×24 CSS px | passes |
| Accessible name on every control | passes |
| Form controls labelled | passes |
| Heading order without skips | passes |
| `<html lang>` | present |
| Keyboard focus visible | 2px `--ember` outline, offset 2px |
| Horizontal overflow at 375px | none |

Three defects were found and fixed in the pass that produced this document:
the command-palette input had no accessible name; the graph's legend and zoom
chips were 19–21.5px tall, under the target-size floor; and two faint tokens
(`--ink-faint` at 10px and 12px) measured 4.28:1 against the sidebar.

### The redesign pass

Re-run against the restructured UI. The handoff's palette asserted AA and did
not hold it, so three tokens are **darker than the values it specified**. The
roles are unchanged; only the weight moved.

| Token | Handoff | Shipped | Why |
| --- | --- | --- | --- |
| `--ink-faint` (light) | `#8b7f71` | `#756a5e` | 3.66:1 on `--bg`, 3.85:1 on `--panel`. Now 4.94 / 5.20 / 4.61 on bg / panel / surface. It carries every section label and every piece of metadata, so it is body text, not decoration. |
| `--kind-uncertain` (light) | `#b3762e` | `#96601f` | 3.35:1 on its own tint. Now 4.66. |
| `--kind-gap` (light) | `#5b7fa6` | `#476888` | 3.60:1 on its own tint. Now 5.03. |
| `--kind-contradiction` (dark) | — | `#dd7a60` | `--oxide` measured 4.31:1 on the contradiction tint. `--oxide` itself is unchanged; the kind token forked. |

`--ink-faint` measures 4.34:1 (light) and 4.16:1 (dark) on `--sunk`. That is
under the floor and deliberate: `--sunk` is the inside of a progress trough and
no text is ever set on it.

Six more defects were found and fixed:

- A review row carried its kind's colour on the whole row rather than on the
  kind's name, so a 13.5px title inherited a decorative hue at 3.5:1.
- The rail's trailing count keeps `--ink-faint` against the rail, but on the
  active nav item — an ink pill — that measured 3.14:1. It takes the pill's own
  foreground there. Worth noting how this was missed on the first pass: the
  count is hidden at zero, and no bench under test had any gaps. **Audit a view
  with data in it**, or the states that only exist with data go unmeasured.
- The filter pills, the TOC entries, the pager, the tree rows, the run-cost
  expander and the search-result titles were 16–22.5px tall, under the 24×24
  pointer-target floor.
- Generated pages set their sections with `##`, and the reader's fixed heading
  offset rendered those as `<h3>` directly under the page `<h1>` — a skipped
  level. The offset is now derived from the body, so its shallowest heading is
  always `<h2>`.
- The graph's only heading was the selection rail's `<h2>`. It now opens with a
  visually-hidden `<h1>`.
- Two grids (`.stats`, `.type-cards`) used `1fr` tracks, whose automatic minimum
  is their content: one long page title pushed the document past 375px and the
  whole page scrolled sideways. Every track is `minmax(0, 1fr)`.

## Deliberate exceptions

**Dead wikilinks** are drawn at low contrast *with a dashed underline*. Color
alone never conveys that a link points at a page the wiki has not written yet;
the underline carries it, which is what 1.4.1 asks for.

**Inline links in prose** are exempt from the 24×24 target-size rule by
2.5.8's own inline exception. Padding a link inside a sentence to 24px would
break the line rhythm it lives in. Standalone controls — buttons, chips, icon
buttons — are held to the floor.

**Graph nodes** are exempt by 2.5.8's essential exception. A node's radius
encodes how many pages link to it, so a floor on the radius would erase the
encoding, and the spacing between nodes is the layout's own output. Every node
is reachable by keyboard, focusing one selects it, and the selection rail names
it and lists its neighbours as ordinary 24px rows — so nothing on the canvas is
the only route to anything.

## How it is built

- **Semantic first.** Real `<button>`, `<a>`, `<details>`, and `<nav>` rather
  than divs with handlers, so keyboard behavior and roles come from the
  platform instead of being reimplemented.
- **Focus is managed on navigation.** Each view change moves focus to `<main>`
  and resets scroll, the SPA equivalent of a page load, so a screen reader
  lands somewhere meaningful instead of staying on a control that no longer
  exists. Background refreshes are not navigations: the live-run poll on the
  Ingestion view swaps only the run feed, so focus, scroll position, and open
  disclosures survive each tick.
- **Modals trap and restore.** The command palette and the add-source wizard
  capture focus, cycle Tab inside themselves, close on Escape and on backdrop
  click, and return focus to the element that opened them.
- **Keyboard shortcuts yield to typing.** The Reviews inbox answers `↑`/`↓`
  and `A`/`K`/`X` anywhere on the screen, so the handler early-returns on any
  `input`, `textarea`, `select`, or `contenteditable` target — otherwise typing
  "a" into the rail's filter box would resolve whatever review was selected.
  `⌘K` is the deliberate exception: it is the way *out* of a field.
- **Live regions.** Status lines use `role="status"`; toasts announce through
  an `aria-live="polite"` container, so results reach a screen reader without
  stealing focus.
- **Motion is opt-out.** Every transition uses one `--dur` token, zeroed under
  `prefers-reduced-motion`, and JS scrolling checks the same query.
- **Both themes are first-class.** The palette is defined once per scheme and
  both are held to AA; dark mode is not an afterthought with its own contrast
  debt.
- **A skip link** is the first tab stop, so a keyboard user can jump the
  sidebar rather than tabbing through every page in the tree.

## Re-running the audit

There is no automated a11y gate in CI — the UI has no test harness, and adding
a headless browser to run one check would cost more than it returns at this
size. The audit is a browser-console pass over the running UI: for each view,
compute contrast of every text node against its resolved background, measure
the bounding box of every control, and assert every control has a name. Re-run
it when the palette or a component's spacing changes.
