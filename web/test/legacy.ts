// Loads the legacy internal/api/ui/app.js into the test's jsdom window and
// hands back its pure functions, so the characterization fixtures are written
// by -- and checked against -- the code that runs in production today. The
// ported TypeScript is later held to the same fixtures.
//
// app.js is a classic script whose last statement is boot(). That call is cut
// and the rest evaluated as one function body, so its top-level declarations
// become locals the body can return. matchMedia is a parameter because jsdom
// does not implement it and app.js calls it at load.
import { readFileSync } from "node:fs";
import { join } from "node:path";

export const APP_JS = join(import.meta.dirname, "../../internal/api/ui/app.js");

export interface CiteRef {
  path: string;
  ranges: string[];
}

export interface CiteFile {
  key: string;
  path: string;
  ranges: string[];
  num: number;
}

export interface Cites {
  files: CiteFile[];
  byKey: Map<string, CiteFile>;
  next: number;
}

export interface PageSummary {
  slug: string;
  title: string;
  type?: string;
  builtAtRef?: string;
  updated?: string;
}

export interface Freshness {
  key: string;
  word: string;
  why: string;
}

export interface LegacyState {
  pages: PageSummary[];
  slugs: Set<string>;
  refs: string[];
  workspace: string | null;
}

export interface Legacy {
  renderMarkdown(src: string, headingOffset?: number, cites?: Cites): string;
  liftDeck(body: string): { deck: string; rest: string; cites: CiteRef[] };
  flattenDeck(sentence: string): { text: string; cites: CiteRef[] };
  parseCites(inner: string): CiteRef[] | null;
  findCites(text: string): { start: number; end: number; refs: CiteRef[] }[];
  nearestPages(slug: string, limit: number): PageSummary[];
  freshnessOf(page: PageSummary | null): Freshness;
  newCites(): Cites;
  state: LegacyState;
  route(): void;
}

const EXPORTS = ["renderMarkdown", "liftDeck", "flattenDeck", "parseCites", "findCites",
  "nearestPages", "freshnessOf", "newCites", "state", "route"] as const;

const stubMatchMedia = (query: string) => ({
  matches: false, media: query,
  addEventListener() { /* never changes */ }, removeEventListener() { /* never changes */ },
});

export const loadLegacy = (): Legacy => evaluateLegacy(readFileSync(APP_JS, "utf8"));

export function evaluateLegacy(src: string): Legacy {
  const tail = /\nboot\(\);\s*$/;
  // If app.js stops ending in boot(), evaluating it would start the whole app
  // (or skip something the loader no longer knows about): fail loudly.
  if (!tail.test(src)) throw new Error("app.js no longer ends with `boot();`; update test/legacy.ts");
  // eslint-disable-next-line @typescript-eslint/no-implied-eval -- evaluating app.js is this loader's purpose
  const body = new Function("matchMedia", `${src.replace(tail, "\n")}\nreturn { ${EXPORTS.join(", ")} };`) as
    (mm: typeof stubMatchMedia) => Legacy;
  return body(stubMatchMedia);
}
