// What each characterization suite feeds a renderer and keeps of its answer.
// The fixture generator and the tests share this, so the JSON is always read
// the way it was written. A port is checked by giving it the same inputs and
// expecting the stored outputs.
import * as cases from "./fixtures/cases";
import type { Legacy, PageSummary } from "./legacy";

export interface Bench {
  now: string;
  pages: PageSummary[];
  refs: string[];
}

export const BENCH: Bench = { now: cases.NOW, pages: cases.PAGES, refs: cases.REFS };

// setBench puts the fixture bench into the legacy state. Fresh arrays every
// time: app.js caches titles keyed on the identity of state.pages.
export function setBench(L: Legacy, bench: Bench, refs?: string[]): void {
  L.state.pages = bench.pages.map((p) => ({ ...p }));
  L.state.slugs = new Set(bench.pages.map((p) => p.slug));
  L.state.refs = [...(refs ?? bench.refs)];
}

type Input = Record<string, unknown>;

interface Suite {
  inputs: Input[];
  run(L: Legacy, input: Input, bench: Bench): unknown;
}

const str = (v: unknown): string => {
  if (typeof v !== "string") throw new Error(`fixture input is not a string: ${JSON.stringify(v)}`);
  return v;
};

export const SUITES: Record<string, Suite> = {
  markdown: {
    inputs: cases.MARKDOWN,
    run(L, input) {
      const cites = L.newCites();
      const offset = typeof input.headingOffset === "number" ? input.headingOffset : 1;
      const html = L.renderMarkdown(str(input.src), offset, cites);
      return { html, cites: cites.files.map(({ path, ranges, num }) => ({ path, ranges, num })) };
    },
  },
  deck: {
    inputs: cases.DECKS,
    run: (L, input) => L.liftDeck(str(input.body)),
  },
  flatten: {
    inputs: cases.FLATTEN,
    run: (L, input) => L.flattenDeck(str(input.sentence)),
  },
  parseCites: {
    inputs: cases.PARSE_CITES.map((inner) => ({ inner })),
    run: (L, input) => L.parseCites(str(input.inner)),
  },
  findCites: {
    inputs: cases.FIND_CITES.map((text) => ({ text })),
    run: (L, input) => L.findCites(str(input.text)),
  },
  nearest: {
    inputs: cases.NEAREST,
    run: (L, input) => L.nearestPages(str(input.slug), Number(input.limit)).map((p) => p.slug),
  },
  freshness: {
    inputs: cases.FRESHNESS,
    run(L, input, bench) {
      setBench(L, bench, Array.isArray(input.refs) ? input.refs.map(str) : undefined);
      return L.freshnessOf(input.page as PageSummary | null);
    },
  },
};

export interface FixtureFile {
  generatedFrom: string;
  bench: Bench;
  cases: (Input & { output: unknown })[];
}
