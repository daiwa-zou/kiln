# Accessibility

The reading UI targets **WCAG 2.2 level AA**. This records what that means
here, what was measured, and the two places the standard is met by a
deliberate exception rather than by the obvious rule.

## Audited

Measured in a browser against the running UI, across every view (overview,
index, graph, gaps, ingestion, reviews, steering, members, log), in both light
and dark themes:

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

## Deliberate exceptions

**Dead wikilinks** are drawn at low contrast *with a dashed underline*. Color
alone never conveys that a link points at a page the wiki has not written yet;
the underline carries it, which is what 1.4.1 asks for.

**Inline links in prose** are exempt from the 24×24 target-size rule by
2.5.8's own inline exception. Padding a link inside a sentence to 24px would
break the line rhythm it lives in. Standalone controls — buttons, chips, icon
buttons — are held to the floor.

## How it is built

- **Semantic first.** Real `<button>`, `<a>`, `<details>`, and `<nav>` rather
  than divs with handlers, so keyboard behavior and roles come from the
  platform instead of being reimplemented.
- **Focus is managed on navigation.** Each view change moves focus to `<main>`
  and resets scroll, the SPA equivalent of a page load, so a screen reader
  lands somewhere meaningful instead of staying on a control that no longer
  exists.
- **Modals trap and restore.** The command palette and the add-source wizard
  capture focus, cycle Tab inside themselves, close on Escape and on backdrop
  click, and return focus to the element that opened them.
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
